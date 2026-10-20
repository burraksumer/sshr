// Package sanitize removes bytes from fetched content that a terminal would
// interpret as commands. Only sshr's own renderer may emit escape sequences.
package sanitize

import "strings"

// Text strips control characters from s, keeping newlines and tabs. It removes
// C0 controls (including ESC), DEL, C1 controls, and Unicode bidi overrides
// that can make text display differently from what it contains.
func Text(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r == '\r':
			return -1
		case r < 0x20, r == 0x7f:
			return -1
		case r >= 0x80 && r <= 0x9f:
			return -1
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return -1
		case r == 0xfeff:
			return -1
		}
		return r
	}, s)
}

// Line is like Text but also collapses newlines and tabs to spaces, for titles
// and other single-line fields.
func Line(s string) string {
	s = Text(s)
	return strings.Join(strings.Fields(s), " ")
}
