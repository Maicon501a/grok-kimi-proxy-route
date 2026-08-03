package warp

import "testing"

func TestShouldFailover(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{name: "internal envelope", status: 500, body: `{"type":"error","error":{"message":"Internal server error"}}`, want: true},
		{name: "internal snake case", status: 500, body: `{"error":"internal_server_error"}`, want: true},
		{name: "upstream error", status: 500, body: `{"message":"upstream error"}`, want: true},
		{name: "validation 500", status: 500, body: `{"message":"invalid model"}`, want: false},
		{name: "rate limited", status: 429, body: `{"error":"rate limit"}`, want: true},
		{name: "bad gateway", status: 502, body: `bad gateway`, want: true},
		{name: "unauthorized", status: 401, body: `unauthorized`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldFailover(tt.status, []byte(tt.body)); got != tt.want {
				t.Fatalf("ShouldFailover(%d, %q) = %v, want %v", tt.status, tt.body, got, tt.want)
			}
		})
	}
}
