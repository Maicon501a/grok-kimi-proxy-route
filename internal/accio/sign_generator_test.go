package accio

import (
	"strings"
	"testing"
)

// realSigns is a sample of captured pctb-x-sign values (corpus + fresh probe).
// Signs 0..2 share one session; signs 3..6 share another (different session).
var realSigns = []string{
	// session A (fresh probe, 2026-08-04)
	"wzpCVE002xAALbpeYUtWHvafiAQKTbpds9WH28rm1icJbJBVA8zq+jhvWOHYUcjcdr5qY5XX1wVh744JORz+Wevp6e26Tbpduk26Xb",
	"wzpCVE002xAAIKOv3BGuA+hTG2UzgKOgqiieJtMbz9oQkYmoGjHzByGSQRzBrNEhb0Nznowqzvh4Epf0IOHnpPIU8BCjsKOgo7CjoK",
	"wzpCVE002xAAI3lCJSWRVWssIvZJc3lDcMtExQn4FTnKclNLwNIp5Ptxm/8bTwvCtaCpfVbJFBui8U0X+gI9Ryj3KvN5U3lDeVN5Q3",
	// session B (corpus captures)
	"wzpCVE002xAALZVgAhQntFjn84UVfZVtnOWo6+XW+RcmXL9lLPvhyRcTz18tQQpZ2gctLejNWxuO36E5FizRacTZxt2VfZVtlX2VbZ",
	"wzpCVE002xAAIpshx59NYZzNgGl7ApsikqqmpOuZ91goE7EqIrTvhhlcwRAjDgQW1EgjYuaCVVSAkK92GGPfJsqWyJKbApsimwKbIp",
	"wzpCVE002xAAKhTgFtmaQGHDyztk+hTqHWIpbGRReJCn2z7irXxQ2xcMJKv341YJFWpIm1Ke7llfWCC+l6tQ7kVeR1oU+hTqFPoU6h",
	"wzpCVE002xAAJPewfuyUmMDYac0nlPe0/jzKMocPm85Ehd28TiKzhfRSx/UUvbVX9jSrxbHADQe8BsPgdPWzsKYApAT3lPe095T3tP",
}

func decodePayload(t *testing.T, sign string) []byte {
	t.Helper()
	raw, err := decodeSignPayload(sign)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return raw
}

func TestRealSignsStructure(t *testing.T) {
	for i, sign := range realSigns {
		payload := decodePayload(t, sign)
		if err := ValidateStructure(payload); err != nil {
			t.Errorf("sign %d structure: %v", i, err)
		}
	}
}

func TestExtractBodyStableAcrossSigns(t *testing.T) {
	// Sessions: 0..2 = probe, 3..4 = sg_dump, 5..6 = sg_fine.
	sessionOf := func(i int) int {
		switch {
		case i <= 2:
			return 0
		case i <= 4:
			return 3
		default:
			return 5
		}
	}
	var first []byte
	for i, sign := range realSigns {
		body, err := ExtractSignBody(sign)
		if err != nil {
			t.Fatalf("extract %d: %v", i, err)
		}
		if i == 0 {
			first = body
			continue
		}
		// Fixed regions (0..12 and 34..43) are SDK constants, identical
		// across sessions.
		for j := 0; j < signBodyLen; j++ {
			if j >= signBodyVarStart && j < signBodyVarEnd {
				continue
			}
			if body[j] != first[j] {
				t.Errorf("sign %d fixed body byte %d differs: %02x vs %02x",
					i, j, body[j], first[j])
			}
		}
		// Same-session signs must share the variable region too.
		refIdx := sessionOf(i)
		if i != refIdx {
			ref, _ := ExtractSignBody(realSigns[refIdx])
			for j := signBodyVarStart; j < signBodyVarEnd; j++ {
				if body[j] != ref[j] {
					t.Errorf("sign %d same-session variable byte %d differs", i, j)
				}
			}
		}
	}
}

func TestGeneratorProducesValidPayloads(t *testing.T) {
	body, err := ExtractSignBody(realSigns[0])
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	g, err := NewSignGenerator(body)
	if err != nil {
		t.Fatalf("generator: %v", err)
	}
	counts := map[byte]int{}
	for i := 0; i < 40; i++ {
		sign, err := g.Generate()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if !strings.HasPrefix(sign, signMarker) {
			t.Fatalf("marker missing")
		}
		payload := decodePayload(t, sign)
		if err := ValidateStructure(payload); err != nil {
			t.Fatalf("generated payload %d: %v", i, err)
		}
		// Extracted body must round-trip.
		got, err := ExtractSignBody(sign)
		if err != nil {
			t.Fatalf("re-extract: %v", err)
		}
		for j := range body {
			if got[j] != body[j] {
				t.Fatalf("body round-trip mismatch at %d", j)
			}
		}
		// Counter must advance mod 16.
		k := (payload[12] ^ payload[14]) >> 4
		counts[k]++
	}
	if len(counts) != 16 {
		t.Fatalf("expected 16 distinct counter values, got %d", len(counts))
	}
	for k := byte(0); k < 16; k++ {
		if counts[k] < 2 || counts[k] > 3 {
			t.Fatalf("counter %d seen %d times (expected ~2-3)", k, counts[k])
		}
	}
}

func TestGeneratorBodiesDifferAcrossSessions(t *testing.T) {
	bodyA, _ := ExtractSignBody(realSigns[0])
	bodyB, _ := ExtractSignBody(realSigns[3]) // different session
	diff := 0
	for i := range bodyA {
		if i >= signBodyVarStart && i < signBodyVarEnd && bodyA[i] != bodyB[i] {
			diff++
		}
	}
	if diff == 0 {
		t.Fatalf("expected session-variable body region to differ")
	}
}
