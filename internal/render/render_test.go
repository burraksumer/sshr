package render

import (
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
