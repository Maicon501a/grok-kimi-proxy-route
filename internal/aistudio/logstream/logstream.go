// Package logstream captura a saída do logger padrão em memória e expõe
// histórico + assinatura em tempo real para o dashboard admin (SSE).
//
// O primeiro acesso a Default() instala um tee no logger padrão: as linhas
// continuam indo para o stderr original e também entram no buffer circular.
package logstream

import (
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// Entry é uma linha de log capturada.
type Entry struct {
	Time time.Time `json:"time"`
	Line string    `json:"line"`
}

const (
	maxHistory   = 500
	maxLineBytes = 4096
)

// Stream é um buffer circular de logs com pub/sub para ouvintes SSE.
// Safe para uso concorrente.
type Stream struct {
	mu      sync.Mutex
	history []Entry
	subs    map[chan Entry]struct{}
	pending strings.Builder
}

var (
	defaultOnce sync.Once
	defaultInst *Stream
)

// Default retorna o stream global, instalando o tee no logger padrão na
// primeira chamada.
func Default() *Stream {
	defaultOnce.Do(func() {
		defaultInst = &Stream{subs: make(map[chan Entry]struct{})}
		log.SetOutput(io.MultiWriter(os.Stderr, defaultInst))
	})
	return defaultInst
}

// Write implementa io.Writer; acumula bytes até fechar linhas e publica
// cada linha completa.
func (s *Stream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending.Write(p)
	for {
		text := s.pending.String()
		idx := strings.IndexByte(text, '\n')
		if idx < 0 {
			break
		}
		line := strings.TrimRight(text[:idx], "\r")
		s.pending.Reset()
		s.pending.WriteString(text[idx+1:])
		s.publishLocked(line)
	}
	return len(p), nil
}

// Flush publica qualquer linha parcial ainda pendente.
func (s *Stream) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending.Len() == 0 {
		return
	}
	line := s.pending.String()
	s.pending.Reset()
	s.publishLocked(line)
}

// Publish registra uma linha manualmente.
func (s *Stream) Publish(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishLocked(line)
}

func (s *Stream) publishLocked(line string) {
	if len(line) > maxLineBytes {
		line = line[:maxLineBytes]
	}
	entry := Entry{Time: time.Now().UTC(), Line: line}
	s.history = append(s.history, entry)
	if len(s.history) > maxHistory {
		s.history = append([]Entry(nil), s.history[len(s.history)-maxHistory:]...)
	}
	for ch := range s.subs {
		select {
		case ch <- entry:
		default:
			// Ouvinte lento: descarta para não bloquear o logger.
		}
	}
}

// History retorna uma cópia do histórico atual.
func (s *Stream) History() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Entry(nil), s.history...)
}

// Subscribe retorna um canal com novas entradas e uma função de cancelamento.
func (s *Stream) Subscribe() (<-chan Entry, func()) {
	ch := make(chan Entry, 64)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	cancel := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
	}
	return ch, cancel
}
