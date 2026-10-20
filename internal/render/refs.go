package render

import (
	"html"
	"regexp"
	"strings"

	"github.com/burraksumer/sshr/internal/sanitize"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// Numeric character references, with or without the closing semicolon, the
// way html.UnescapeString reads them.
var numericRef = regexp.MustCompile(`&#(?:[xX][0-9a-fA-F]+|[0-9]+);?`)

// escapeControlRefs neutralizes character references that decode to control
// characters. Sanitizing fetched content removes raw control bytes, but
// glamour decodes references like "&#27;" while rendering text, code spans,
// strikethrough and HTML, which would turn them back into ESC. Rewriting the
// "&" as "&amp;" makes glamour show the reference literally instead.
//
// Code blocks are left alone, since glamour prints them without decoding.
func escapeControlRefs(p parser.Parser, src []byte) []byte {
	if !numericRef.Match(src) {
		return src
	}
	var code [][2]int
	doc := p.Parse(text.NewReader(src))
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n.(type) {
		case *ast.FencedCodeBlock, *ast.CodeBlock:
			lines := n.Lines()
			for i := range lines.Len() {
				s := lines.At(i)
				code = append(code, [2]int{s.Start, s.Stop})
			}
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	inCode := func(i int) bool {
		for _, r := range code {
			if i >= r[0] && i < r[1] {
				return true
			}
		}
		return false
	}

	var b strings.Builder
	last := 0
	for _, m := range numericRef.FindAllIndex(src, -1) {
		if inCode(m[0]) || !strings.ContainsFunc(html.UnescapeString(string(src[m[0]:m[1]])), sanitize.Dropped) {
			continue
		}
		b.Write(src[last:m[0]])
		b.WriteString("&amp;")
		b.Write(src[m[0]+1 : m[1]])
		last = m[1]
	}
	if last == 0 {
		return src
	}
	b.Write(src[last:])
	return []byte(b.String())
}
