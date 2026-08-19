package httpserver

import "testing"

func TestEffectiveToolCallingMode(t *testing.T) {
	tests := []struct {
		name       string
		requested  string
		configured string
		want       string
	}{
		{name: "native default", want: "native_first"},
		{name: "configured native", configured: "native_first", want: "native_first"},
		{name: "configured bridge", configured: "bridge_first", want: "bridge_first"},
		{name: "request bridge override", requested: "bridge_first", configured: "native_first", want: "bridge_first"},
		{name: "request native override", requested: "native_first", configured: "bridge_first", want: "native_first"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveToolCallingMode(tt.requested, tt.configured); got != tt.want {
				t.Fatalf("effectiveToolCallingMode(%q, %q) = %q, want %q", tt.requested, tt.configured, got, tt.want)
			}
		})
	}
}
