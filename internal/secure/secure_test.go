package secure

import "testing"

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := "sk-deepseek-test-1234567890abcdef"
	enc, err := Encrypt(key)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if enc == "" || enc == key {
		t.Fatalf("encrypt produced unusable output: %q", enc)
	}
	dec, err := Decrypt(enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if dec != key {
		t.Fatalf("round-trip mismatch: got %q want %q", dec, key)
	}
}

func TestEncryptEmpty(t *testing.T) {
	enc, err := Encrypt("")
	if err != nil || enc != "" {
		t.Fatalf("empty encrypt: enc=%q err=%v", enc, err)
	}
	dec, err := Decrypt("")
	if err != nil || dec != "" {
		t.Fatalf("empty decrypt: dec=%q err=%v", dec, err)
	}
}

func TestHasCiphertext(t *testing.T) {
	if HasCiphertext("") {
		t.Fatal("empty should not look encrypted")
	}
	if HasCiphertext("sk-plaintext") {
		t.Fatal("plaintext should not look encrypted")
	}
	enc, _ := Encrypt("secret")
	if !HasCiphertext(enc) {
		t.Fatalf("encrypted blob should be detected: %q", enc)
	}
}

func TestDecryptTamperedFails(t *testing.T) {
	enc, err := Encrypt("secret")
	if err != nil {
		t.Fatal(err)
	}
	tampered := enc[:len(enc)-4] + "AAAA"
	if _, err := Decrypt(tampered); err == nil {
		t.Fatal("tampered blob should fail to decrypt")
	}
}
