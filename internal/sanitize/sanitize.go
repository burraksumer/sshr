// Package sanitize removes bytes from fetched content that a terminal would
// interpret as commands. Only sshr's own renderer may emit escape sequences.
package sanitize

import "strings"

// Dropped reports whether r is a character Text removes. That covers C0
// controls other than newline and tab, so ESC and CR go, plus DEL, C1
// controls, Unicode bidi overrides and the byte order mark.
func Dropped(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case r < 0x20, r == 0x7f:
		return true
	case r >= 0x80 && r <= 0x9f:
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	case r == 0xfeff:
		return true
	}
	return false
}

// Text strips the characters Dropped reports from s. Bidi overrides go too,
// because they can make text display differently from what it contains.
func Text(s string) string {
	return strings.Map(func(r rune) rune {
		if Dropped(r) {
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
