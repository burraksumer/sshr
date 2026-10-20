// Package render turns markdown into terminal output. It numbers links the way
// lynx does: "text [3]" in the body and a list of URLs at the end.
package render

import (
	"bytes"
	"fmt"
	"strings"

	"charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Link is a numbered link from a rendered document.
type Link struct {
	N   int
	URL string
}

// Document is rendered markdown and the links it contains.
type Document struct {
	Content string
	Links   []Link
}

// Options controls rendering.
type Options struct {
	Width int
	Dark  bool
	// BaseURL resolves relative links.
	BaseURL string
}

// gm parses markdown. Its parser is safe to share.
var gm = goldmark.New(
	goldmark.WithExtensions(extension.GFM, extension.DefinitionList),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
)

// Markdown renders md for a terminal of the given width.
func Markdown(md string, opts Options) (Document, error) {
	width := max(opts.Width, 20)
	style := styles.LightStyleConfig
	if opts.Dark {
		style = styles.DarkStyleConfig
	}
	// The document margin comes from the style. Wrap inside it.
	margin := 0
	if style.Document.Margin != nil {
		margin = int(*style.Document.Margin)
	}

	source := []byte(md)
	doc := gm.Parser().Parse(text.NewReader(source))
	// Number links first, so a strikethrough that cleanTree flattens keeps
	// the [n] markers of the links inside it.
	var links []Link
	source, links = numberLinks(doc, source, opts.BaseURL)
	if len(links) > 0 {
		appendLinkList(doc, &source, links)
	}
	source = cleanTree(doc, source)

	r := renderer.NewRenderer(renderer.WithNodeRenderers(util.Prioritized(ansi.NewRenderer(ansi.Options{
		BaseURL:  opts.BaseURL,
		WordWrap: width - 2*margin,
		Styles:   style,
		// Table links get numbered like the rest instead of getting their
		// own per-table footer.
		InlineTableLinks: true,
	}), 1000)))
	var buf bytes.Buffer
	if err := r.Render(&buf, source, doc); err != nil {
		return Document{}, err
	}
	out := filterOutput(buf.String(), hyperlinkTargets(doc, source))
	return Document{Content: strings.TrimRight(out, "\n"), Links: links}, nil
}

// numberLinks rewrites every inline link so it renders as its text followed by
// " [n]", and returns the link list. Text for new nodes is appended to source,
// so existing node segments stay valid.
func numberLinks(doc ast.Node, source []byte, base string) ([]byte, []Link) {
	var links []Link
	byURL := map[string]int{}
	// Links and bare URLs, in reading order, so numbers follow the text.
	var targets []ast.Node
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Link:
			targets = append(targets, n)
			return ast.WalkSkipChildren, nil
		case *ast.AutoLink:
			if n.AutoLinkType == ast.AutoLinkURL {
				targets = append(targets, n)
			}
		}
		return ast.WalkContinue, nil
	})
	for _, n := range targets {
		var dest string
		switch n := n.(type) {
		case *ast.Link:
			if dest = linkDestination(n.Destination); dest == "" || strings.HasPrefix(dest, "#") {
				continue
			}
		case *ast.AutoLink:
			dest = string(n.URL(source))
		}
		dest = resolve(base, dest)
		// A link to a section of this same document, which html-to-markdown
		// makes absolute, isn't worth a number. Show its text only.
		if sameDocument(dest, base) {
			if l, ok := n.(*ast.Link); ok {
				l.Destination = []byte("#")
			}
			continue
		}
		num, ok := byURL[dest]
		if !ok {
			num = len(links) + 1
			byURL[dest] = num
			links = append(links, Link{N: num, URL: dest})
		}
		// An anchor-only destination makes glamour render a link's text
		// without printing the URL after it. Bare URLs show their address
		// already and keep it.
		if l, ok := n.(*ast.Link); ok {
			l.Destination = []byte("#")
		}
		var t *ast.Text
		source, t = appendText(source, fmt.Sprintf(" [%d]", num))
		n.Parent().InsertAfter(n.Parent(), n, t)
	}
	return source, links
}

// appendLinkList adds a "Links" section listing every numbered link.
func appendLinkList(doc ast.Node, source *[]byte, links []Link) {
	var t *ast.Text
	h := ast.NewHeading(2)
	*source, t = appendText(*source, "Links")
	h.AppendChild(h, t)
	doc.AppendChild(doc, ast.NewThematicBreak())
	doc.AppendChild(doc, h)

	list := ast.NewList('-')
	list.IsTight = true
	for _, l := range links {
		item := ast.NewListItem(2)
		block := ast.NewTextBlock()
		*source, t = appendText(*source, fmt.Sprintf("[%d] ", l.N))
		block.AppendChild(block, t)
		start := len(*source)
		*source = append(*source, l.URL...)
		al := ast.NewAutoLink(ast.AutoLinkURL, ast.NewTextSegment(text.NewSegment(start, len(*source))))
		block.AppendChild(block, al)
		item.AppendChild(item, block)
		list.AppendChild(list, item)
	}
	doc.AppendChild(doc, list)
}

func appendText(source []byte, s string) ([]byte, *ast.Text) {
	start := len(source)
	source = append(source, s...)
	return source, ast.NewTextSegment(text.NewSegment(start, len(source)))
}
