package ui

import (
	"bytes"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"charm.land/bubbles/v2/viewport"

	tea "charm.land/bubbletea/v2"
	"github.com/burraksumer/sshr/internal/content"
	"github.com/burraksumer/sshr/internal/fetch"
	"github.com/burraksumer/sshr/internal/images"
)

func key(s string) tea.KeyPressMsg {
	switch s {
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	}
	return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
}

func TestBackspaceEditsLinkNumber(t *testing.T) {
	m := Model{screen: screenReader, entry: &content.Entry{}, st: newStyles(true)}
	var tm tea.Model = m
	for _, k := range []string{"1", "2", "backspace"} {
		tm, _ = tm.(Model).Update(key(k))
	}
	got := tm.(Model)
	if got.screen != screenReader || got.digits != "1" {
		t.Errorf("screen %v digits %q, want reader and \"1\"", got.screen, got.digits)
	}
}

func TestResizeRendersOnceSettled(t *testing.T) {
	m := Model{screen: screenReader, entry: &content.Entry{Body: "hello"}, st: newStyles(true), width: 80, height: 24}
	var tm tea.Model = m
	var cmds []tea.Cmd
	for _, w := range []int{60, 61, 62} {
		var cmd tea.Cmd
		tm, cmd = tm.(Model).Update(tea.WindowSizeMsg{Width: w, Height: 24})
		cmds = append(cmds, cmd)
	}
	// Only the last resize's tick may render.
	for i, cmd := range cmds {
		msg := cmd()
		next, _ := tm.(Model).Update(msg)
		rendered := next.(Model).doc.Content != ""
		if want := i == len(cmds)-1; rendered != want {
			t.Errorf("resize %d rendered = %v, want %v", i, rendered, want)
		}
	}
}

func TestLoadingScreenOnlyQuits(t *testing.T) {
	m := Model{st: newStyles(true)}
	tm, _ := m.Update(key("/"))
	if tm.(Model).filtering {
		t.Error("search opened on the loading screen")
	}
	if _, cmd := tm.(Model).Update(key("q")); cmd == nil {
		t.Error("q did not quit on the loading screen")
	}
}

func TestOpenClearsSearchFocus(t *testing.T) {
	m := Model{st: newStyles(true), filtering: true, entry: nil}
	m.open(&content.Entry{Body: "x"}, false)
	if m.filtering {
		t.Error("search still focused after opening an entry")
	}
}

// A post that fits on screen, read from the top, stays at the top after a
// resize makes it longer than the screen.
func TestResizeKeepsTopOfShortPost(t *testing.T) {
	body := strings.Repeat("word ", 120)
	m := Model{st: newStyles(true), width: 120, height: 40}
	m.vp = viewport.New()
	m.open(&content.Entry{Body: body}, false)
	var tm tea.Model = m
	tm, cmd := tm.(Model).Update(tea.WindowSizeMsg{Width: 30, Height: 8})
	tm, _ = tm.(Model).Update(cmd())
	got := tm.(Model)
	if off := got.vp.YOffset(); off != 0 {
		t.Errorf("offset after resize = %d, want 0", off)
	}
}

func TestPasteIntoSearch(t *testing.T) {
	m := New(content.NewStore(&content.Discoverer{}, nil), nil)
	m.site = &content.Site{Entries: []*content.Entry{{Title: "golang notes"}, {Title: "other"}}}
	m.applyFilter()
	var tm tea.Model = m
	tm, _ = tm.(Model).Update(key("/"))
	tm, _ = tm.(Model).Update(tea.PasteMsg{Content: "golang"})
	got := tm.(Model)
	if got.filter.Value() != "golang" || len(got.visible) != 1 {
		t.Errorf("filter %q, %d visible", got.filter.Value(), len(got.visible))
	}
}

// Going back after following a link returns to the reading position.
func TestBackKeepsPosition(t *testing.T) {
	long := &content.Entry{URL: "https://x.example/a", Body: strings.Repeat("line\n\n", 300)}
	other := &content.Entry{URL: "https://x.example/b", Body: "short"}
	m := Model{st: newStyles(true), width: 80, height: 20, vp: viewport.New()}
	m.open(long, false)
	m.vp.SetYOffset(400)
	before := m.vp.YOffset()
	m.open(other, true)
	m.back()
	if got := m.vp.YOffset(); got < before-2 || got > before+2 {
		t.Errorf("offset after back = %d, want about %d", got, before)
	}
}

// A large body renders off the event loop and shows once it arrives.
func TestLargeBodyRendersInBackground(t *testing.T) {
	big := &content.Entry{URL: "https://x.example/big", Body: strings.Repeat("word ", syncRender/4)}
	m := Model{st: newStyles(true), width: 80, height: 20, vp: viewport.New()}
	cmd := m.open(big, false)
	if cmd == nil {
		t.Fatal("large body rendered inline")
	}
	if len(m.doc.Content) != 0 {
		t.Error("document shown before the render finished")
	}
	next, _ := m.Update(cmd())
	if got := next.(Model); len(got.doc.Content) == 0 {
		t.Error("render never shown")
	}
}

// A resize while a large post renders keeps the position the render will
// open at.
func TestResizeDuringPendingRenderKeepsPosition(t *testing.T) {
	big := &content.Entry{URL: "https://x.example/big2", Body: strings.Repeat("word ", syncRender/4)}
	m := Model{st: newStyles(true), width: 80, height: 20, vp: viewport.New()}
	m.entry = big
	m.screen = screenReader
	m.renderEntry(0.7)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 70, Height: 20})
	if got := tm.(Model).keepPos; got != 0.7 {
		t.Errorf("keepPos = %v, want 0.7", got)
	}
}

// A resize that settles after opening another entry doesn't move it to the
// old entry's position.
func TestResizeDoesNotLeakToNextEntry(t *testing.T) {
	a := &content.Entry{URL: "https://x.example/a", Body: strings.Repeat("line\n\n", 300)}
	b := &content.Entry{URL: "https://x.example/b", Body: strings.Repeat("line\n\n", 300)}
	m := Model{st: newStyles(true), width: 80, height: 20, vp: viewport.New()}
	m.open(a, false)
	m.vp.SetYOffset(300)
	tm, cmd := m.Update(tea.WindowSizeMsg{Width: 70, Height: 20})
	got := tm.(Model)
	got.open(b, true)
	tm, _ = got.Update(cmd())
	got = tm.(Model)
	if off := got.vp.YOffset(); off != 0 {
		t.Errorf("entry b opened at offset %d, want 0", off)
	}
}

func TestImageViewer(t *testing.T) {
	var b bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	png.Encode(&b, img)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cat.png" {
			w.Write(b.Bytes())
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	loader := images.New(fetch.New(fetch.Options{Hosts: []string{u.Host}, Rate: 1000, Burst: 100}))

	entry := &content.Entry{URL: srv.URL + "/post", Body: "A ![white cat](/cat.png) and ![gone](/gone.png)."}
	m := Model{st: newStyles(true), width: 60, height: 20, vp: viewport.New(), images: loader, site: &content.Site{}}
	m.open(entry, false)
	if len(m.doc.Links) != 2 || !m.doc.Links[0].Image {
		t.Fatalf("links = %+v", m.doc.Links)
	}
	var tm tea.Model = m
	tm, _ = tm.(Model).Update(key("1"))
	tm, cmd := tm.(Model).Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := tm.(Model)
	if got.screen != screenImage || !strings.Contains(got.viewImage(), "Loading") {
		t.Fatalf("screen %v:\n%s", got.screen, got.viewImage())
	}
	tm, _ = got.Update(cmd())
	got = tm.(Model)
	view := got.viewImage()
	if len(got.picLines) == 0 || !strings.Contains(view, "white cat") || !strings.Contains(view, "8×8 png") || !strings.Contains(view, "▀") {
		t.Errorf("viewer:\n%s", view)
	}
	if lines := strings.Count(view, "\n") + 1; lines != got.height {
		t.Errorf("viewer has %d lines, want %d", lines, got.height)
	}
	tm, _ = got.Update(tea.WindowSizeMsg{Width: 30, Height: 12})
	if l := tm.(Model).picLines; len(l) > 12-picChrome {
		t.Errorf("%d image lines after a resize", len(l))
	}
	tm, _ = tm.(Model).Update(key("h"))
	if got := tm.(Model); got.screen != screenReader || got.picImg != nil {
		t.Errorf("back went to %v", got.screen)
	}

	// A missing image says so.
	got = tm.(Model)
	got.width = 80
	cmd = got.follow(2)
	tm, _ = got.Update(cmd())
	if view := tm.(Model).viewImage(); !strings.Contains(view, "Couldn't load the image") || !strings.Contains(view, "404") {
		t.Errorf("missing image:\n%s", view)
	}

	// With images off, the number shows the image's address.
	m.images = nil
	m.follow(1)
	if m.screen != screenReader || !strings.Contains(m.status, "/cat.png") {
		t.Errorf("images off: screen %v, status %q", m.screen, m.status)
	}
}
