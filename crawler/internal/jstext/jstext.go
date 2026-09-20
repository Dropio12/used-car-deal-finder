// Package jstext holds the few JavaScript string rules the port has to copy
// exactly: what counts as whitespace, how trim() behaves, and how offsets are
// counted (UTF-16 code units, not bytes).
package jstext

import (
	"strconv"
	"strings"
	"unicode/utf16"
)

// SpaceClass is JavaScript's `\s` as a Go regexp character-class body.
// Go's own `\s` is ASCII-only, JavaScript's is not.
const SpaceClass = `\t\n\x0B\f\r \x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}`

// IsSpace reports whether r is whitespace for JavaScript's `\s` and trim().
func IsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// Trim is String.prototype.trim.
func Trim(s string) string {
	return strings.TrimFunc(s, IsSpace)
}

// UTF16Index converts a byte offset in s to a UTF-16 code-unit offset.
func UTF16Index(s string, byteIndex int) int {
	n := 0
	for _, r := range s[:byteIndex] {
		n += utf16.RuneLen(r)
	}
	return n
}

// SliceUTF16 is String.prototype.slice(start, end) with UTF-16 offsets.
func SliceUTF16(s string, start, end int) string {
	units := utf16.Encode([]rune(s))
	if start < 0 {
		start = 0
	}
	if end > len(units) {
		end = len(units)
	}
	if start >= end {
		return ""
	}
	return string(utf16.Decode(units[start:end]))
}

// Number is JavaScript's String(n) for the numbers this crawler handles.
func Number(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
