package proxyhttp

import (
	"testing"

	"grok-desktop/internal/accio"
)

func TestApplyAccioDefaultReasoningEffortUsesHighestModelValue(t *testing.T) {
	body := map[string]any{}
	models := []accio.Model{{
		ID:               "accio/model-a",
		ReasoningEfforts: []string{"low", "medium", "high"},
	}}

	if !applyAccioDefaultReasoningEffort(body, "accio/model-a", models) {
		t.Fatal("expected default effort to be applied")
	}
	if got := body["reasoning_effort"]; got != "high" {
		t.Fatalf("reasoning_effort = %#v, want high", got)
	}
}

func TestApplyAccioDefaultReasoningEffortPreservesClientChoice(t *testing.T) {
	models := []accio.Model{{
		ID:               "accio/model-a",
		ReasoningEfforts: []string{"low", "high"},
	}}
	for name, body := range map[string]map[string]any{
		"openai field":     {"reasoning_effort": "low"},
		"accio field":      {"reasoningEffort": "medium"},
		"responses object": {"reasoning": map[string]any{"effort": "low"}},
	} {
		t.Run(name, func(t *testing.T) {
			if applyAccioDefaultReasoningEffort(body, "accio/model-a", models) {
				t.Fatal("explicit client effort must not be replaced")
			}
		})
	}
}

func TestApplyAccioDefaultReasoningEffortSkipsUnknownModel(t *testing.T) {
	body := map[string]any{}
	models := []accio.Model{{
		ID:               "accio/model-a",
		ReasoningEfforts: []string{"low", "high"},
	}}
	if applyAccioDefaultReasoningEffort(body, "accio/model-missing", models) {
		t.Fatal("unknown models must keep gateway default")
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Fatalf("unexpected effort: %#v", body)
	}
}
