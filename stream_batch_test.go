package main

import (
	"sync"
	"testing"

	"grok-desktop/internal/upstream"
)

func TestChatEventBatcherPreservesTextBeforeControlEvents(t *testing.T) {
	var mu sync.Mutex
	var got []upstream.StreamEvent
	b := newChatEventBatcher(func(ev upstream.StreamEvent) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	})
	b.Push(upstream.StreamEvent{Type: "content", Text: "A"})
	b.Push(upstream.StreamEvent{Type: "content", Text: "B"})
	b.Push(upstream.StreamEvent{Type: "usage", Usage: &upstream.Usage{TotalTokens: 2}})
	b.Close()

	if len(got) != 3 {
		t.Fatalf("events=%d, want first token + batched text + usage: %#v", len(got), got)
	}
	if got[0].Type != "content" || got[0].Text != "A" {
		t.Fatalf("first event=%#v", got[0])
	}
	if got[1].Type != "content" || got[1].Text != "B" {
		t.Fatalf("batched event=%#v", got[1])
	}
	if got[2].Type != "usage" {
		t.Fatalf("control event moved or lost: %#v", got[2])
	}
}

func TestChatEventBatcherCloseDrainsBufferedText(t *testing.T) {
	var mu sync.Mutex
	var text string
	b := newChatEventBatcher(func(ev upstream.StreamEvent) {
		if ev.Type != "content" {
			return
		}
		mu.Lock()
		text += ev.Text
		mu.Unlock()
	})
	b.Push(upstream.StreamEvent{Type: "content", Text: "one"})
	b.Push(upstream.StreamEvent{Type: "content", Text: "two"})
	b.Close()

	mu.Lock()
	defer mu.Unlock()
	if text != "onetwo" {
		t.Fatalf("text=%q, want onetwo", text)
	}
}
