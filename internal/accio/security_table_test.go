package accio

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestGenerateSecurityTableMatchesCapturedSDK(t *testing.T) {
	table := generateSecurityTable(0x6a7184c5)
	if got, want := hex.EncodeToString(table[:16]), "650137280a747b44391b233379767f13"; got != want {
		t.Fatalf("first 16 bytes = %s, want %s", got, want)
	}

	hash := sha256.Sum256(table[:])
	if got, want := hex.EncodeToString(hash[:]), "c2fa2498d7751e197ee96e557fc424c1a2d529915d72000b845ea157e3f798f4"; got != want {
		t.Fatalf("table SHA-256 = %s, want %s", got, want)
	}
}
