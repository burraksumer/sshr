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

// sameDocument reports whether u points into the document at base, differing
// at most in the fragment.
func sameDocument(u, base string) bool {
	a, err1 := url.Parse(u)
	b, err2 := url.Parse(base)
	if err1 != nil || err2 != nil || a.Fragment == "" {
		return false
	}
	a.Fragment, a.RawFragment, b.Fragment, b.RawFragment = "", "", "", ""
	return a.String() == b.String()
}

// resolve returns ref resolved against base, with every byte that isn't
// printable ASCII percent-encoded.
func resolve(base, ref string) string {
	if base == "" {
		return sanitize.URL(ref)
	}
	b, err := url.Parse(base)
	if err != nil {
		return sanitize.URL(ref)
	}
	r, err := url.Parse(ref)
	if err != nil {
		return sanitize.URL(ref)
	}
	return sanitize.URL(b.ResolveReference(r).String())
}
