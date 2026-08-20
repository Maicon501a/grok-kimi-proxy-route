package googlebg

import (
	"encoding/base64"
	"strings"
)

var std = base64.StdEncoding
var raw = base64.RawStdEncoding

func atob(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	if b, err := std.DecodeString(s); err == nil {
		return string(b)
	}
	if b, err := raw.DecodeString(strings.TrimRight(s, "=")); err == nil {
		return string(b)
	}
	return ""
}

func btoa(s string) string {
	return std.EncodeToString([]byte(s))
}
