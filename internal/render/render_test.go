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

var evilLink = regexp.MustCompile("\x1b]8;[^\x07\x1b]*evil")

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
		// References split by inline markup inside strikethrough, which
		// glamour decodes from the joined text of its children.
		"~~&`#`27;]8;;https://evil.example/&`#`7;official docs&`#`27;]8;;&`#`7;~~",
		"~~&[#](#a)27;[31mX~~",
		"~~&![#](x.png)27;[31mX~~",
		"~~&`#2`7;[31mX~~ and ~~&`#x`1b;[31mX~~",
		"~~&`#`8238;evil~~ bidi",
		"| a | b |\n|---|---|\n| &#27;[31m | [x](https://x.example) |",
		"<p>&amp;#27;[31m</p>",
		"raw OSC 8 with an id: &#27;]8;id=1;https://evil.example&#7;x",
	}
	for _, p := range payloads {
		doc, err := Markdown(p, Options{Width: 80, Dark: true})
		if err != nil {
			t.Fatal(err)
		}
		// Glamour's own output is SGR styling plus OSC 8 links it builds
		// itself, which always carry an id. Remove those; no other OSC
		// sequence, BEL, C1 control or bidi override may be left, and no
		// link may point at the attacker's host.
		if evilLink.MatchString(doc.Content) {
			t.Errorf("%q: hyperlink to evil.example:\n%q", p, doc.Content)
		}
		out := glamourLinks.ReplaceAllString(doc.Content, "")
		for _, bad := range []string{"\x1b]", "\x07", "\x1b[2J", "\x1b[31m", "\x1b[5m", "\u009d", "\u009b", "\u202e"} {
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

func TestFilterOutput(t *testing.T) {
	targets := map[string]bool{"https://ok.example": true}
	in := "a\x1b[38;5;252mb\x1b[m" + // SGR kept
		"\x1b]8;id=1;https://ok.example\x07ok\x1b]8;;\x07" + // known link kept
		"\x1b]8;id=2;https://evil.example\x07bad\x1b]8;;\x07" + // unknown link dropped
		"\x1b]0;title\x07\x1b]52;c;aGk=\x1b\\" + // other OSC dropped
		"\x1b[2J\x1b[H\x1bc" + // other CSI and ESC sequences dropped
		"\u009b31m‮\x07\x1b" // C1, bidi, BEL, lone ESC dropped
	want := "a\x1b[38;5;252mb\x1b[m\x1b]8;id=1;https://ok.example\x07ok\x1b]8;;\x07bad\x1b]8;;\x0731m"
	if got := filterOutput(in, targets); got != want {
		t.Errorf("filterOutput:\n got %q\nwant %q", got, want)
	}
}

func TestTableLinksNumbered(t *testing.T) {
	md := "See [Go](https://go.dev).\n\n| name | link |\n|---|---|\n| spec | [spec](https://example.com/spec) |\n"
	doc, err := Markdown(md, Options{Width: 80})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Links) != 2 || doc.Links[1].URL != "https://example.com/spec" {
		t.Fatalf("links = %+v", doc.Links)
	}
	if plain := ansi.Strip(doc.Content); !strings.Contains(plain, "spec [2]") {
		t.Errorf("table link not numbered [2]:\n%s", plain)
	}
}

func TestLinkDestinationsDecoded(t *testing.T) {
	doc, err := Markdown(`[a](/posts/a\_b) and [b](/s?x=1&amp;y=2) and [c](/&#27;evil)`, Options{Width: 80, BaseURL: "https://site.example/"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://site.example/posts/a_b", "https://site.example/s?x=1&y=2", "https://site.example/evil"}
	for i, l := range doc.Links {
		if i >= len(want) || l.URL != want[i] {
			t.Errorf("links = %+v, want %q", doc.Links, want)
			break
		}
	}
}

func TestOnlyWebSchemesClickable(t *testing.T) {
	md := "<file:///etc/passwd> <vscode://x/y> ![i](javascript:alert(1)) [d](ms-msdt:/id) <https://ok.example> <me@example.com>"
	doc, err := Markdown(md, Options{Width: 120})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"\x1b]8;id=[0-9]+;file:", "vscode:", "javascript:", "ms-msdt:"} {
		if regexp.MustCompile("\x1b]8;[^\x07]*" + bad).MatchString(doc.Content) {
			t.Errorf("clickable %s link in output", bad)
		}
	}
	for _, good := range []string{"https://ok.example", "mailto:me@example.com"} {
		if !strings.Contains(doc.Content, ";"+good+"\x07") {
			t.Errorf("%s is not clickable", good)
		}
	}
	if !strings.Contains(ansi.Strip(doc.Content), "file:///etc/passwd") {
		t.Error("non-web link should still be shown as text")
	}
}

func TestFilterDropsStrayBytes(t *testing.T) {
	if got := filterOutput("a\x1b\xc2\x9b2Jb\x9bc", nil); got != "a2Jbc" {
		t.Errorf("filterOutput = %q", got)
	}
}

// A C1 control in the base URL's query must not reach the output, whether
// in a link target or as text.
func TestC1InBaseURL(t *testing.T) {
	base := "https://example.com/post?a=\u009c\u009b2J\u009d0;pwned\u009c"
	doc, err := Markdown("click [here](//) now", Options{Width: 80, BaseURL: base})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"\u009b", "\u009c", "\u009d"} {
		if strings.Contains(doc.Content, r) {
			t.Errorf("output contains %q:\n%q", r, doc.Content)
		}
		for _, l := range doc.Links {
			if strings.Contains(l.URL, r) {
				t.Errorf("link URL contains %q: %q", r, l.URL)
			}
		}
	}
}

func TestAllowedOSCRejectsNonASCII(t *testing.T) {
	targets := map[string]bool{"https://x.example/?a=\u009c": true}
	if allowedOSC("8;id=1;https://x.example/?a=\u009c", targets) {
		t.Error("OSC with a C1 control was allowed")
	}
}

func TestDocCacheEvicts(t *testing.T) {
	c := newDocCache(90)
	doc := func(n int) Document { return Document{Content: strings.Repeat("x", n)} }
	c.put(cacheKey{md: "a"}, doc(20))
	c.put(cacheKey{md: "b"}, doc(20))
	c.get(cacheKey{md: "a"}) // a is now most recent
	c.put(cacheKey{md: "c"}, doc(20))
	c.put(cacheKey{md: "d"}, doc(20))
	c.put(cacheKey{md: "e"}, doc(20)) // over 90 bytes, b goes first
	if _, ok := c.get(cacheKey{md: "b"}); ok {
		t.Error("least recently used entry kept")
	}
	if _, ok := c.get(cacheKey{md: "a"}); !ok {
		t.Error("recently used entry evicted")
	}
	c.put(cacheKey{md: "huge"}, doc(30))
	if _, ok := c.get(cacheKey{md: "huge"}); ok {
		t.Error("entry over a quarter of the cache was stored")
	}
}

func TestLinksInStrikethroughNumbered(t *testing.T) {
	doc, err := Markdown("~~see [the old docs](https://example.com/v1)~~", Options{Width: 80})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Links) != 1 || !strings.Contains(ansi.Strip(doc.Content), "the old docs [1]") {
		t.Errorf("links %+v, content:\n%s", doc.Links, ansi.Strip(doc.Content))
	}
}
