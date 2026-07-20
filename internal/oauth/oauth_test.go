package oauth

import (
	"errors"
	"testing"
)

func TestIsInvalidGrant(t *testing.T) {
	if !IsInvalidGrant(errors.New(`invalid_grant: Refresh token has been revoked`)) {
		t.Fatal("expected true")
	}
	if !IsInvalidGrant(errors.New(`token expirado — faça login de novo: invalid_grant: Refresh token has been revoked`)) {
		t.Fatal("expected true for wrapped")
	}
	if IsInvalidGrant(errors.New("timeout talking to auth.x.ai")) {
		t.Fatal("expected false")
	}
	if IsInvalidGrant(nil) {
		t.Fatal("nil")
	}
}
