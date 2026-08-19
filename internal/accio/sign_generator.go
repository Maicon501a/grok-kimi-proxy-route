package accio

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// signPayloadLen is the decoded size of the pctb-x-sign payload (67 bytes).
const signPayloadLen = 67

// signMarker is the fixed 12-char prefix of every pctb-x-sign.
const signMarker = "wzpCVE002xAA"

// signBodyFixedPrefixLen / suffix describe the stable regions of the 44-byte
// session body (positions 0..12 and 34..43 are SDK constants; 13..33 are
// derived from the session/UMID and stable within a session).
const (
	signBodyLen          = 44
	signBodyVarStart     = 13
	signBodyVarEnd       = 34 // exclusive
	signBodyFixedPrefix  = 13
	signBodyFixedSuffix  = 10
	signBodyFixedSuffix0 = 34
)

// SignGenerator produces pctb-x-sign values without the native addon.
//
// Layout of the 67-byte payload (recovered by tracing the Windows
// SecurityGuardSDK64 runtime, validated against 50 real captures):
//
//	[0]   A      = 0x20 | (Z & 0x0f)
//	[1]   X      (mirrored at 13, 59, 61, 63, 65)
//	[2:12] fields (10 bytes, per-request random)
//	[12]  Y      = Z ^ (counter<<4)
//	[13]  X
//	[14]  Z      (mirrored at 62, 66)
//	[15:59] session body (44 bytes) XOR-masked: even idx ^ Z, odd idx ^ X
//	[59]  X
//	[60]  Z ^ 0x10   (mirrored at 64)
//	[61]  X
//	[62]  Z
//	[63]  X
//	[64]  Z ^ 0x10
//	[65]  X
//	[66]  Z
type SignGenerator struct {
	body  [signBodyLen]byte
	count byte // counter 0..15, incremented per generated sign
}

// NewSignGenerator seeds the generator with the 44-byte session body. The
// body is stable within a session (positions 13..33 vary across sessions) and
// can be extracted from any real pctb-x-sign via ExtractSignBody.
func NewSignGenerator(body []byte) (*SignGenerator, error) {
	if len(body) != signBodyLen {
		return nil, fmt.Errorf("sign body must be %d bytes, got %d", signBodyLen, len(body))
	}
	g := &SignGenerator{}
	copy(g.body[:], body)
	return g, nil
}

// Generate returns the next pctb-x-sign for the current session. X, Z and the
// 10 field bytes are random; the counter advances by one per call (mod 16).
func (g *SignGenerator) Generate() (string, error) {
	g.count = (g.count + 1) & 0x0f
	k := g.count

	var payload [signPayloadLen]byte

	// Random components.
	var x, z byte
	var fields [10]byte
	randBytes := make([]byte, 12)
	if _, err := rand.Read(randBytes); err != nil {
		return "", err
	}
	x = randBytes[0]
	z = randBytes[1]
	copy(fields[:], randBytes[2:])

	// Header.
	payload[0] = 0x20 | (z & 0x0f) // A
	payload[1] = x
	copy(payload[2:12], fields[:])
	payload[12] = z ^ (k << 4) // Y (counter embedded)
	payload[13] = x
	payload[14] = z

	// Masked session body.
	for i := 15; i < 15+signBodyLen; i++ {
		b := g.body[i-15]
		if i%2 == 0 {
			payload[i] = b ^ z
		} else {
			payload[i] = b ^ x
		}
	}

	// Tail mirrors.
	payload[59] = x
	payload[60] = z ^ 0x10 // tail counter field j=1 (dominant in captures)
	payload[61] = x
	payload[62] = z
	payload[63] = x
	payload[64] = z ^ 0x10
	payload[65] = x
	payload[66] = z

	return signMarker + base64.StdEncoding.EncodeToString(payload[:]), nil
}

// decodeSignPayload decodes the base64 payload of a sign, adding the missing
// padding (the wire format omits it: 90 chars = 67 bytes + 2 pad).
func decodeSignPayload(sign string) ([]byte, error) {
	if len(sign) < len(signMarker) || sign[:len(signMarker)] != signMarker {
		return nil, fmt.Errorf("invalid sign marker")
	}
	b64 := sign[len(signMarker):]
	switch len(b64) % 4 {
	case 2:
		b64 += "=="
	case 3:
		b64 += "="
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("sign payload decode: %w", err)
	}
	return raw, nil
}

// ExtractSignBody recovers the 44-byte session body from a real pctb-x-sign.
// The body is the XOR-unmasked region [15:59].
func ExtractSignBody(sign string) ([]byte, error) {
	raw, err := decodeSignPayload(sign)
	if err != nil {
		return nil, err
	}
	if len(raw) != signPayloadLen {
		return nil, fmt.Errorf("sign payload length %d != %d", len(raw), signPayloadLen)
	}
	x := raw[1]
	z := raw[14]
	body := make([]byte, signBodyLen)
	for i := 0; i < signBodyLen; i++ {
		p := 15 + i
		if p%2 == 0 {
			body[i] = raw[p] ^ z
		} else {
			body[i] = raw[p] ^ x
		}
	}
	return body, nil
}

// ValidateStructure checks every recovered invariant of a 67-byte payload.
// It is used by tests to confirm generated payloads are indistinguishable
// from real ones.
func ValidateStructure(payload []byte) error {
	if len(payload) != signPayloadLen {
		return fmt.Errorf("payload length %d", len(payload))
	}
	x := payload[1]
	z := payload[14]
	y := payload[12]
	// X mirrors.
	for _, i := range []int{13, 59, 61, 63, 65} {
		if payload[i] != x {
			return fmt.Errorf("X mirror failed at %d", i)
		}
	}
	// Z mirrors.
	for _, i := range []int{62, 66} {
		if payload[i] != z {
			return fmt.Errorf("Z mirror failed at %d", i)
		}
	}
	// Tail Y mirrors (b60 == b64, sharing Z's low nibble; the high nibble
	// carries a secondary counter that real captures show as 1 in the
	// majority of samples).
	if payload[60] != payload[64] {
		return fmt.Errorf("Y tail mirror failed at 60/64")
	}
	if (payload[60]^z)&0x0f != 0 {
		return fmt.Errorf("Y tail low nibble mismatch")
	}
	// A = 0x20 | low nibble of Z.
	if payload[0] != 0x20|(z&0x0f) {
		return fmt.Errorf("A mismatch: %02x vs %02x", payload[0], 0x20|(z&0x0f))
	}
	// Y shares low nibble with Z.
	if (y^z)&0x0f != 0 {
		return fmt.Errorf("Y/Z low nibble mismatch")
	}
	return nil
}
