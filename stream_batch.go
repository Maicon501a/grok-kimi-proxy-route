package main

import (
	"strings"
	"sync"
	"time"

	"grok-desktop/internal/upstream"
)

// chatEventBatcher keeps the Wails bridge responsive during fast token streams.
// Text tokens are coalesced for at most one frame; control events (usage, done,
// tools and errors) always flush pending text first and keep their order.
type chatEventBatcher struct {
	in     chan upstream.StreamEvent
	closed chan struct{}
	onEmit func(upstream.StreamEvent)
	once   sync.Once
}

func newChatEventBatcher(onEmit func(upstream.StreamEvent)) *chatEventBatcher {
	b := &chatEventBatcher{
		in:     make(chan upstream.StreamEvent, 128),
		closed: make(chan struct{}),
		onEmit: onEmit,
	}
	go b.run()
	return b
}

func (b *chatEventBatcher) Push(ev upstream.StreamEvent) {
	b.in <- ev
}

func (b *chatEventBatcher) Close() {
	b.once.Do(func() { close(b.in) })
	<-b.closed
}

func (b *chatEventBatcher) run() {
	defer close(b.closed)
	ticker := time.NewTicker(16 * time.Millisecond)
	defer ticker.Stop()

	var pending *upstream.StreamEvent
	var text strings.Builder
	flushText := func() {
		if pending == nil || text.Len() == 0 {
			return
		}
		pending.Text = text.String()
		b.onEmit(*pending)
		text.Reset()
	}

	for {
		select {
		case ev, ok := <-b.in:
			if !ok {
				flushText()
				return
			}
			if ev.Type != "content" && ev.Type != "thinking" {
				flushText()
				pending = nil
				b.onEmit(ev)
				continue
			}
			if pending != nil && pending.Type != ev.Type {
				flushText()
				pending = nil
			}
			if pending == nil {
				cp := ev
				pending = &cp
				// Do not delay the first visible token of a phase.
				b.onEmit(ev)
				continue
			}
			pending.ID = ev.ID
			pending.Model = ev.Model
			pending.Account = ev.Account
			pending.Email = ev.Email
			text.WriteString(ev.Text)
			// The first token is emitted immediately; subsequent chunks are
			// frame-batched. This preserves TTFT while reducing bridge traffic.
			if text.Len() >= 4096 {
				flushText()
			}
		case <-ticker.C:
			flushText()
		}
	}
}
