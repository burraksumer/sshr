package render

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestNumberedLinks(t *testing.T) {
	md := "See [the docs](/docs) and [Go](https://go.dev), then [the docs](/docs) again.\n\n```\n[not](a link)\n```\n"
	doc, err := Markdown(md, Options{Width: 80, Dark: true, BaseURL: "https://example.com/post"})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Links) != 2 {
		t.Fatalf("got %d links, want 2: %+v", len(doc.Links), doc.Links)
	}
	if doc.Links[0].URL != "https://example.com/docs" || doc.Links[1].URL != "https://go.dev" {
		t.Errorf("links = %+v", doc.Links)
	}
	plain := ansi.Strip(doc.Content)
	for _, want := range []string{"the docs [1]", "Go [2]", "again", "[1] https://example.com/docs", "[2] https://go.dev", "[not](a link)"} {
		if !strings.Contains(plain, want) {
			t.Errorf("output missing %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "the docs [1] https://") {
		t.Errorf("URL printed inline:\n%s", plain)
	}
}

func TestWrapsToWidth(t *testing.T) {
	md := strings.Repeat("word ", 200)
	doc, err := Markdown(md, Options{Width: 40})
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(doc.Content, "\n") {
		if w := ansi.StringWidth(line); w > 40 {
			t.Fatalf("line is %d cells wide, want <= 40: %q", w, ansi.Strip(line))
		}
	}
}

var glamourLinks = regexp.MustCompile("\x1b]8;id=[0-9]+;[^\x07]*\x07|\x1b]8;;\x07")

// Character references that decode to control characters must not reach the
// terminal as escape sequences, in any node type glamour decodes.
func TestControlReferences(t *testing.T) {
	payloads := []string{
		"plain &#27;[31mred&#27;[0m text",
		"no semicolon &#27[2J here",
		"hex &#x1b;]8;;https://evil.example&#x07;click&#x1b;]8;;&#x07;",
		"code span `&#27;[31m`",
		"~~strike &#27;[31m~~",
		"inline <b>&#27;[31m</b> html",
		"<div>\n&#27;]0;title&#7;\n</div>",
		"C1 &#157;0;title&#7; and &#155;31m",
		"emphasis *&#27;[5mblink*",
		"![alt &#27;[31m](https://example.com/i.png)",
	}
	for _, p := range payloads {
		doc, err := Markdown(p, Options{Width: 80, Dark: true})
		if err != nil {
			t.Fatal(err)
		}
		// Glamour's own output is SGR styling plus OSC 8 links it builds
		// itself, which always carry an id. Remove those; no other OSC
		// sequence, BEL or C1 control may be left.
		out := glamourLinks.ReplaceAllString(doc.Content, "")
		for _, bad := range []string{"\x1b]", "\x07", "\x1b[2J", "\x1b[31m", "\x1b[5m", "\u009d", "\u009b"} {
			if strings.Contains(out, bad) {
				t.Errorf("%q: output contains %q:\n%q", p, bad, out)
			}
		}
	}
}

func TestControlReferencesShownLiterally(t *testing.T) {
	doc, err := Markdown("text &#27; and `&#27;` and ok &#233;\n\n```\n&#27; in a block\n```\n", Options{Width: 80})
	if err != nil {
		t.Fatal(err)
	}
	plain := ansi.Strip(doc.Content)
	if strings.Count(plain, "&#27;") != 3 || !strings.Contains(plain, "é") {
		t.Errorf("references not shown as written:\n%s", plain)
	}
}
