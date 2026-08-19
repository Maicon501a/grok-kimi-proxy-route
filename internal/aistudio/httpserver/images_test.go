package httpserver

import "testing"

func TestNormalizeImageAspectRatio(t *testing.T) {
	tests := []struct {
		input string
		want  string
		ok    bool
	}{
		{input: "", want: "", ok: true},
		{input: "auto", want: "", ok: true},
		{input: "Auto", want: "", ok: true},
		{input: "16:9", want: "16:9", ok: true},
		{input: "1:1", want: "1:1", ok: true},
		{input: "7:3", want: "", ok: false},
	}
	for _, tc := range tests {
		got, ok := normalizeImageAspectRatio(tc.input)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("normalizeImageAspectRatio(%q) = (%q, %v), want (%q, %v)", tc.input, got, ok, tc.want, tc.ok)
		}
	}
}
