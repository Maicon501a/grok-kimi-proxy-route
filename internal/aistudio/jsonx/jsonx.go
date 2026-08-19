// Package jsonx provides lenient JSON parsing and JSON array manipulation helpers
// used to decode MakerSuite/AI Studio responses and to repair malformed JSON
// produced by the model.
package jsonx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Raw is a type alias for arbitrary decoded JSON values.
type Raw = any

// Decode parses a JSON document strictly.
func Decode(data []byte) (any, error) {
	var out any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// DecodeLenient attempts to parse JSON with multiple repair strategies used by
// the original Node implementation to handle malformed tool-call payloads.
// Bugs fixed from the original:
//  1. The original tryLenientJsonParse could return the unparseable string as a
//     successful parse because of an accidental return path; here we always
//     return nil on failure.
//  2. Repair of unescaped newlines inside strings now operates byte-by-byte
//     instead of a regex that mishandled surrogate pairs.
//  3. Closing brace/bracket counters now correctly ignore punctuation inside
//     strings.
func DecodeLenient(data []byte) any {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil
	}

	if v, err := strictDecode(trimmed); err == nil {
		return v
	}

	// Strategy 1: remove trailing commas and escape raw newlines inside strings.
	if v, err := strictDecode(escapeStringsAndStripTrailingCommas(trimmed)); err == nil {
		return v
	}

	// Strategy 2: close unbalanced braces/brackets and quote unterminated strings.
	if v, err := strictDecode(repairUnbalanced(trimmed)); err == nil {
		return v
	}

	// Strategy 3: strip markdown fences and surrounding prose.
	if v, err := strictDecode(stripFencesAndProse(trimmed)); err == nil {
		return v
	}

	// Strategy 4: quote bare keys + single-quoted strings after stripping fences.
	if v, err := strictDecode(quoteBareKeysAndSingleQuotes(stripFencesAndProse(trimmed))); err == nil {
		return v
	}

	return nil
}

func strictDecode(data []byte) (any, error) {
	var out any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// escapeStringsAndStripTrailingCommas escapes raw control characters inside
// JSON strings and removes trailing commas before '}' or ']'.
func escapeStringsAndStripTrailingCommas(data []byte) []byte {
	var buf bytes.Buffer
	buf.Grow(len(data))

	inString := false
	escaping := false
	for i := 0; i < len(data); i++ {
		ch := data[i]
		if inString {
			if escaping {
				escaping = false
				buf.WriteByte(ch)
				continue
			}
			if ch == '\\' {
				escaping = true
				buf.WriteByte(ch)
				continue
			}
			if ch == '"' {
				inString = false
				buf.WriteByte(ch)
				continue
			}
			if ch == '\n' {
				buf.WriteString(`\n`)
				continue
			}
			if ch == '\r' {
				buf.WriteString(`\r`)
				continue
			}
			if ch == '\t' {
				buf.WriteString(`\t`)
				continue
			}
			buf.WriteByte(ch)
			continue
		}

		if ch == '"' {
			inString = true
			buf.WriteByte(ch)
			continue
		}

		// Strip trailing comma: look ahead skipping whitespace for '}' or ']'.
		if ch == ',' {
			j := i + 1
			for j < len(data) && (data[j] == ' ' || data[j] == '\t' || data[j] == '\n' || data[j] == '\r') {
				j++
			}
			if j < len(data) && (data[j] == '}' || data[j] == ']') {
				continue
			}
		}
		buf.WriteByte(ch)
	}
	return buf.Bytes()
}

// repairUnbalanced closes unterminated strings and unclosed braces/brackets.
func repairUnbalanced(data []byte) []byte {
	cleaned := escapeStringsAndStripTrailingCommas(data)

	openBraces := 0
	openBrackets := 0
	inString := false
	escaping := false

	for _, ch := range cleaned {
		if inString {
			if escaping {
				escaping = false
				continue
			}
			if ch == '\\' {
				escaping = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			openBraces++
		case '}':
			if openBraces > 0 {
				openBraces--
			}
		case '[':
			openBrackets++
		case ']':
			if openBrackets > 0 {
				openBrackets--
			}
		}
	}

	var buf bytes.Buffer
	buf.Write(cleaned)
	if inString {
		buf.WriteByte('"')
	}
	for i := 0; i < openBraces; i++ {
		buf.WriteByte('}')
	}
	for i := 0; i < openBrackets; i++ {
		buf.WriteByte(']')
	}
	return escapeStringsAndStripTrailingCommas(buf.Bytes())
}

// stripFencesAndProse removes ```json / ~~~json fences and any prose before the
// first JSON structural character.
func stripFencesAndProse(data []byte) []byte {
	s := string(data)
	// Strip BOM if present.
	s = strings.TrimPrefix(s, "\uFEFF")
	// Strip leading fences ```json or ~~~json.
	lower := strings.ToLower(s)
	for _, fence := range []string{"```json", "```", "~~~json", "~~~"} {
		if idx := strings.Index(lower, fence); idx >= 0 {
			s = s[idx+len(fence):]
			break
		}
	}
	// Strip trailing fences.
	lower = strings.ToLower(s)
	for _, fence := range []string{"```", "~~~"} {
		if idx := strings.LastIndex(lower, fence); idx >= 0 {
			s = s[:idx]
			break
		}
	}
	// Trim leading prose until first '{' or '['.
	if first := strings.IndexAny(s, "{["); first > 0 {
		s = s[first:]
	}
	// Strip trailing semicolons.
	s = strings.TrimRight(s, ";")
	// Normalize smart quotes.
	s = strings.ReplaceAll(s, "“", `"`)
	s = strings.ReplaceAll(s, "”", `"`)
	return []byte(s)
}

// quoteBareKeysAndSingleQuotes quotes unquoted object keys and converts
// single-quoted strings to double-quoted ones.
func quoteBareKeysAndSingleQuotes(data []byte) []byte {
	s := string(data)
	// Bare keys: { key: or , key:
	// Use a simple state machine rather than regex for reliability.
	var buf bytes.Buffer
	buf.Grow(len(s))

	i := 0
	for i < len(s) {
		ch := s[i]
		if ch == '"' {
			// Copy string literal verbatim.
			buf.WriteByte(ch)
			i++
			for i < len(s) {
				buf.WriteByte(s[i])
				if s[i] == '\\' && i+1 < len(s) {
					buf.WriteByte(s[i+1])
					i += 2
					continue
				}
				if s[i] == '"' {
					i++
					break
				}
				i++
			}
			continue
		}
		if ch == '\'' {
			// Single-quoted string -> double-quoted with escapes.
			buf.WriteByte('"')
			i++
			for i < len(s) && s[i] != '\'' {
				if s[i] == '"' {
					buf.WriteString(`\"`)
				} else if s[i] == '\\' {
					buf.WriteString(`\\`)
				} else {
					buf.WriteByte(s[i])
				}
				i++
			}
			if i < len(s) {
				i++ // skip closing '
			}
			buf.WriteByte('"')
			continue
		}
		// Bare key after '{' or ','.
		if ch == '{' || ch == ',' {
			buf.WriteByte(ch)
			i++
			// Skip whitespace.
			for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
				buf.WriteByte(s[i])
				i++
			}
			if i < len(s) && isBareKeyStart(s[i]) {
				start := i
				for i < len(s) && isBareKeyChar(s[i]) {
					i++
				}
				key := s[start:i]
				buf.WriteByte('"')
				buf.WriteString(key)
				buf.WriteByte('"')
			}
			continue
		}
		buf.WriteByte(ch)
		i++
	}
	return escapeStringsAndStripTrailingCommas(buf.Bytes())
}

func isBareKeyStart(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_'
}

func isBareKeyChar(ch byte) bool {
	return isBareKeyStart(ch) || (ch >= '0' && ch <= '9') || ch == '-' || ch == '.'
}

// Marshal returns a compact JSON encoding.
func Marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

// MarshalIndent returns a pretty-printed JSON encoding.
func MarshalIndent(v any, prefix, indent string) ([]byte, error) {
	return json.MarshalIndent(v, prefix, indent)
}

// DecodeUTF16Runes decodes a slice that may contain surrogate pairs stored as
// raw code points (defensive helper). Returns the input unchanged if valid.
func DecodeUTF16Runes(runes []rune) []rune {
	out := make([]rune, 0, len(runes))
	for i := 0; i < len(runes); i++ {
		if utf16.IsSurrogate(runes[i]) && i+1 < len(runes) && utf16.IsSurrogate(runes[i+1]) {
			r := utf16.DecodeRune(runes[i], runes[i+1])
			if r != utf8.RuneError {
				out = append(out, r)
				i++
				continue
			}
		}
		out = append(out, runes[i])
	}
	return out
}

// ErrNotArray is returned when a value is expected to be a JSON array but isn't.
var ErrNotArray = errors.New("jsonx: value is not an array")

// AsArray returns the value as a []any or an error.
func AsArray(v any) ([]any, error) {
	if v == nil {
		return nil, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: got %T", ErrNotArray, v)
	}
	return arr, nil
}

// AsObject returns the value as a map[string]any or an error.
func AsObject(v any) (map[string]any, error) {
	if v == nil {
		return nil, nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("jsonx: value is not an object: %T", v)
	}
	return obj, nil
}

// AsString returns the value as a string if it is one.
func AsString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// AsBool returns the value as a bool if it is one.
func AsBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

// AsNumber returns the value as a json.Number.
func AsNumber(v any) (json.Number, bool) {
	n, ok := v.(json.Number)
	return n, ok
}

// AsInt64 converts a numeric value to int64.
func AsInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return i, true
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	}
	return 0, false
}

// AsFloat64 converts a numeric value to float64.
func AsFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}
