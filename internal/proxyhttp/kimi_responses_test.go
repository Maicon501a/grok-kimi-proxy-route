package proxyhttp

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPipeKimiResponsesSSECapacityBusyFailsWithUpstreamMessage(t *testing.T) {
	body := "data: {\"error\":{\"message\":\"Too many people are chatting with Kimi right now. Please try again soon.\"}}\n\n"
	rec := httptest.NewRecorder()
	err := pipeKimiChatSSEToResponsesContext(context.Background(), rec, strings.NewReader(body), "k3-agent-xhigh")
	if err == nil || !strings.Contains(err.Error(), "sse quota error") {
		t.Fatalf("want quota error, got %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "response.failed") || !strings.Contains(out, "quota_exhausted") {
		t.Fatalf("expected failed quota event, got:\n%s", out)
	}
	if !strings.Contains(out, "Too many people are chatting with Kimi") {
		t.Fatalf("Kimi message was not propagated:\n%s", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Fatalf("capacity error must not become a completed empty response:\n%s", out)
	}
}

func TestPipeKimiResponsesSSEForwardsNonQuotaError(t *testing.T) {
	body := "data: {\"error\":{\"message\":\"Kimi rejected this request because its session is unavailable.\"}}\n\n"
	rec := httptest.NewRecorder()
	err := pipeKimiChatSSEToResponsesContext(context.Background(), rec, strings.NewReader(body), "k3-agent-xhigh")
	if err == nil || !strings.Contains(err.Error(), "sse upstream error") {
		t.Fatalf("want upstream error, got %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "upstream_error") || !strings.Contains(out, "Kimi rejected this request") {
		t.Fatalf("expected propagated upstream error, got:\n%s", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Fatalf("upstream error must not become a completed empty response:\n%s", out)
	}
}

func TestPipeKimiResponsesSSERejectsSilentEmptyCompletion(t *testing.T) {
	body := "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"finish_reason\":\"stop\"}]}\n\n"
	rec := httptest.NewRecorder()
	err := pipeKimiChatSSEToResponsesContext(context.Background(), rec, strings.NewReader(body), "k3-agent-xhigh")
	if err == nil || !strings.Contains(err.Error(), "sse empty response") {
		t.Fatalf("want empty-response error, got %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "empty_upstream_response") {
		t.Fatalf("expected explicit empty-upstream error, got:\n%s", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Fatalf("empty stream must not become a completed response:\n%s", out)
	}
}
