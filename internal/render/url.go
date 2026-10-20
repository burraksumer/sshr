package render

import (
	"html"
	"net/url"
	"strings"

	"github.com/burraksumer/sshr/internal/sanitize"
	"github.com/yuin/goldmark/util"
)

// linkDestination decodes a raw markdown link destination the way CommonMark
// defines it, resolving backslash escapes and character references, and drops
// any control characters that decoding produced.
func linkDestination(raw []byte) string {
	return strings.TrimSpace(sanitize.Text(html.UnescapeString(string(util.UnescapePunctuations(raw)))))
}

// Clickable reports whether u may become a terminal hyperlink. Only http,
// https and mailto qualify. Others, like file or schemes that launch local
// apps, are shown as plain text.
func Clickable(u string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	switch strings.ToLower(p.Scheme) {
	case "http", "https", "mailto":
		return true
	}
	return false
}

func resolve(base, ref string) string {
	if base == "" {
		return ref
	}
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}
