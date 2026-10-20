package render

import (
	"html"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/burraksumer/sshr/internal/sanitize"
	"github.com/yuin/goldmark/ast"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

// Fetched content arrives with raw control bytes already stripped. Glamour
// then decodes character references such as "&#27;" while rendering, which
// would bring control characters back. cleanTree and filterOutput close that
// gap from both sides.

// cleanTree rewrites the parsed document so that whatever glamour decodes
// comes out without control characters:
//
//   - Text nodes, which glamour decodes one by one, get their decoded value
//     with control characters removed and every "&" written as "&amp;".
//     Glamour's single decode then yields exactly that value, and joining
//     neighbouring nodes can't form a new reference.
//   - Strikethrough is rendered from the joined text of all its children, so
//     it is flattened into a single cleaned Text node.
//   - HTML is passed through bluemonday, which decodes references, so
//     references to control characters in it are escaped.
//
// Code blocks and image alt text are printed without decoding and are left
// alone. New text is appended to source so existing segments stay valid.
func cleanTree(doc ast.Node, source []byte) []byte {
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.FencedCodeBlock, *ast.CodeBlock, *ast.Image:
			return ast.WalkSkipChildren, nil
		case *extast.Strikethrough:
			clean := cleanValue(string(n.Text(source))) //nolint:staticcheck // glamour renders it this way
			n.RemoveChildren(n)
			var t *ast.Text
			source, t = appendText(source, clean)
			n.AppendChild(n, t)
			return ast.WalkSkipChildren, nil
		case *ast.Text:
			v := string(n.Segment.Value(source))
			if clean := cleanValue(v); clean != v {
				start := len(source)
				source = append(source, clean...)
				n.Segment = text.NewSegment(start, len(source))
			}
		case *ast.RawHTML:
			segs := text.NewSegments()
			for i := range n.Segments.Len() {
				var seg text.Segment
				source, seg = cleanHTMLSegment(source, n.Segments.At(i))
				segs.Append(seg)
			}
			n.Segments = segs
		case *ast.HTMLBlock:
			lines := text.NewSegments()
			for i := range n.Lines().Len() {
				var seg text.Segment
				source, seg = cleanHTMLSegment(source, n.Lines().At(i))
				lines.Append(seg)
			}
			n.SetLines(lines)
			if n.HasClosure() {
				source, n.ClosureLine = cleanHTMLSegment(source, n.ClosureLine)
			}
		}
		return ast.WalkContinue, nil
	})
	return source
}

// cleanValue returns text that glamour decodes to v's decoded form, with
// references to control characters shown as written and nothing that
// decodes to a control character.
func cleanValue(v string) string {
	if !strings.Contains(v, "&") {
		return v
	}
	decoded := html.UnescapeString(string(escapeControlRefs([]byte(v))))
	return strings.ReplaceAll(sanitize.Text(decoded), "&", "&amp;")
}

// Numeric character references, with or without the closing semicolon, the
// way html.UnescapeString reads them.
var numericRef = regexp.MustCompile(`&#(?:[xX][0-9a-fA-F]+|[0-9]+);?`)

// escapeControlRefs rewrites references that decode to control characters
// as "&amp;#…", so a decoder shows them literally.
func escapeControlRefs(v []byte) []byte {
	return numericRef.ReplaceAllFunc(v, func(ref []byte) []byte {
		if strings.ContainsFunc(html.UnescapeString(string(ref)), sanitize.Dropped) {
			return append([]byte("&amp;"), ref[1:]...)
		}
		return ref
	})
}

func cleanHTMLSegment(source []byte, seg text.Segment) ([]byte, text.Segment) {
	v := seg.Value(source)
	fixed := escapeControlRefs(v)
	if string(fixed) == string(v) {
		return source, seg
	}
	start := len(source)
	source = append(source, fixed...)
	return source, text.NewSegment(start, len(source))
}

// hyperlinkTargets lists the URLs glamour may legitimately wrap in OSC 8
// hyperlinks: autolinks, including the numbered link list, and images.
func hyperlinkTargets(doc ast.Node, source []byte) map[string]bool {
	targets := map[string]bool{}
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.AutoLink:
			u := string(n.URL(source))
			targets[u] = true
			targets["mailto:"+u] = true
		case *ast.Image:
			targets[string(n.Destination)] = true
		}
		return ast.WalkContinue, nil
	})
	return targets
}

// filterOutput is the last line of defence: it keeps only the escape
// sequences glamour itself produces, SGR styling and OSC 8 hyperlinks to
// targets, and drops every other escape sequence and control character.
func filterOutput(s string, targets map[string]bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) {
			switch s[i+1] {
			case '[':
				j := i + 2
				for j < len(s) && s[j] >= 0x30 && s[j] <= 0x3f {
					j++
				}
				for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2f {
					j++
				}
				if j < len(s) && s[j] >= 0x40 && s[j] <= 0x7e {
					if s[j] == 'm' && strings.Trim(s[i+2:j], "0123456789;:") == "" {
						b.WriteString(s[i : j+1])
					}
					i = j + 1
				} else {
					i = j
				}
				continue
			case ']':
				end, term := len(s), 0
				if k := strings.IndexAny(s[i+2:], "\x07\x1b"); k >= 0 {
					end = i + 2 + k
					switch {
					case s[end] == 0x07:
						term = 1
					case end+1 < len(s) && s[end+1] == '\\':
						term = 2
					}
				}
				if term > 0 && allowedOSC(s[i+2:end], targets) {
					b.WriteString(s[i : end+term])
				}
				i = end + term
				continue
			default:
				i += 2
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if !sanitize.Dropped(r) {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// allowedOSC accepts OSC 8 hyperlinks to known targets and the OSC 8 reset.
func allowedOSC(body string, targets map[string]bool) bool {
	rest, ok := strings.CutPrefix(body, "8;")
	if !ok {
		return false
	}
	_, target, ok := strings.Cut(rest, ";")
	return ok && (target == "" || targets[target])
}
