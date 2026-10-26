package ui

import (
	"bytes"
	"fmt"
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
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi/kitty"
)

func key(s string) tea.KeyPressMsg {
	switch s {
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	}
	return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
}

func TestBackspaceEditsLinkNumber(t *testing.T) {
	m := Model{screen: screenReader, entry: &content.Entry{}, st: newStyles(true, "")}
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
	m := Model{screen: screenReader, entry: &content.Entry{Body: "hello"}, st: newStyles(true, ""), width: 80, height: 24}
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
	m := Model{st: newStyles(true, "")}
	tm, _ := m.Update(key("/"))
	if tm.(Model).filtering {
		t.Error("search opened on the loading screen")
	}
	if _, cmd := tm.(Model).Update(key("q")); cmd == nil {
		t.Error("q did not quit on the loading screen")
	}
}

func TestOpenClearsSearchFocus(t *testing.T) {
	m := Model{st: newStyles(true, ""), filtering: true, entry: nil}
	m.open(&content.Entry{Body: "x"}, false)
	if m.filtering {
		t.Error("search still focused after opening an entry")
	}
}

// A post that fits on screen, read from the top, stays at the top after a
// resize makes it longer than the screen.
func TestResizeKeepsTopOfShortPost(t *testing.T) {
	body := strings.Repeat("word ", 120)
	m := Model{st: newStyles(true, ""), width: 120, height: 40}
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
	m := Model{st: newStyles(true, ""), width: 80, height: 20, vp: viewport.New()}
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
	m := Model{st: newStyles(true, ""), width: 80, height: 20, vp: viewport.New()}
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
	m := Model{st: newStyles(true, ""), width: 80, height: 20, vp: viewport.New()}
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
	m := Model{st: newStyles(true, ""), width: 80, height: 20, vp: viewport.New()}
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
	m := Model{st: newStyles(true, ""), width: 60, height: 20, vp: viewport.New(), images: loader, site: &content.Site{}}
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

func rawOf(t *testing.T, cmd tea.Cmd) string {
	t.Helper()
	if cmd == nil {
		t.Fatal("no command")
	}
	switch msg := cmd().(type) {
	case tea.RawMsg:
		return fmt.Sprint(msg.Msg)
	case tea.BatchMsg:
		for _, c := range msg {
			if c == nil {
				continue
			}
			// The batch's other commands are ticks, which would wait.
			if _, tick := c().(resizedMsg); !tick {
				return rawOf(t, c)
			}
		}
		t.Fatal("no raw output in the batch")
	default:
		t.Fatalf("command gave %T, not raw output", msg)
	}
	return ""
}

// kittyModel is a viewer model whose terminal answered the graphics query,
// reading from a site with one 96 by 64 image.
func kittyModel(t *testing.T, term, name string) (Model, func()) {
	t.Helper()
	var b bytes.Buffer
	png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 96, 64)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(b.Bytes()) }))
	u, _ := url.Parse(srv.URL)
	loader := images.New(fetch.New(fetch.Options{Hosts: []string{u.Host}, Rate: 1000, Burst: 100}))
	m := Model{st: newStyles(true, ""), width: 60, height: 20, vp: viewport.New(), images: loader, site: &content.Site{}}

	var tm tea.Model = m
	tm, _ = tm.Update(tea.EnvMsg{"TERM=" + term})
	tm, _ = tm.Update(tea.TerminalVersionMsg{Name: name})
	tm, _ = tm.Update(uv.KittyGraphicsEvent{Options: kittyOpts(images.KittyQueryID), Payload: []byte("ENOTSUPPORTED")})
	if tm.(Model).kitty.on {
		t.Error("kitty on after an error")
	}
	tm, _ = tm.Update(uv.KittyGraphicsEvent{Options: kittyOpts(images.KittyQueryID), Payload: []byte("OK")})
	m = tm.(Model)
	if !m.kitty.on {
		t.Fatal("kitty off after OK")
	}
	m.open(&content.Entry{URL: srv.URL + "/post", Body: "![cat](/cat.png)"}, false)
	return m, srv.Close
}

// openPic follows the image link and delivers the image and its encoding,
// returning the command that sent the data.
func openPic(t *testing.T, m Model) (Model, tea.Cmd) {
	t.Helper()
	var tm tea.Model = m
	tm, _ = tm.Update(key("1"))
	tm, cmd := tm.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	tm, cmd = tm.Update(cmd()) // the image
	tm, cmd = tm.Update(cmd()) // its encoding
	return tm.(Model), cmd
}

// cmds runs cmd and returns the messages it gives, looking inside batches.
func cmds(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if b, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range b {
			out = append(out, cmds(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

func TestKittyInit(t *testing.T) {
	m := Model{images: images.New(nil)}
	var queried bool
	for _, msg := range cmds(m.Init()) {
		if r, ok := msg.(tea.RawMsg); ok && strings.Contains(fmt.Sprint(r.Msg), "a=q") {
			queried = true
		}
	}
	if !queried {
		t.Error("no query at start")
	}
	if !images.KittyPlaceholders("xterm-256color", "kitty(0.39.1)") || !images.KittyPlaceholders("xterm-256color", "ghostty 1.2.0") || !images.KittyPlaceholders("xterm-ghostty", "") {
		t.Error("kitty or Ghostty not recognized")
	}
	if images.KittyPlaceholders("xterm-256color", "WezTerm 20240203") {
		t.Error("WezTerm taken for placeholders")
	}
}

func TestKittyPlaceholders(t *testing.T) {
	m, done := kittyModel(t, "xterm-256color", "ghostty 1.2.0")
	defer done()
	m, cmd := openPic(t, m)
	if m.kitty.id != 16 || !m.kitty.virtual {
		t.Errorf("id %d, virtual %v", m.kitty.id, m.kitty.virtual)
	}
	// The data goes out with a placement sized like the placeholders.
	out := rawOf(t, cmd)
	if !strings.Contains(out, "a=t,f=100,t=d,i=16") || !strings.HasSuffix(out, images.KittyPlace(16, 12, 4)) {
		t.Errorf("transmission ends %q", out[max(len(out)-60, 0):])
	}
	if len(m.picLines) != 4 || !strings.Contains(m.viewImage(), "\U0010EEEE") || strings.Contains(m.viewImage(), "▀") {
		t.Errorf("viewer:\n%s", m.viewImage())
	}
	// A smaller window moves the image onto fewer cells.
	var tm tea.Model = m
	tm, cmd = tm.Update(tea.WindowSizeMsg{Width: 20, Height: 8})
	if got := rawOf(t, cmd); got != images.KittyPlace(16, 6, 2) {
		t.Errorf("resize sent %q", got)
	}
	// Help hides the placeholders, which hides the image, so nothing is sent.
	tm, cmd = tm.Update(key("?"))
	if cmd != nil {
		t.Error("help sent something")
	}
	tm, _ = tm.Update(key("x"))
	// Leaving frees it, and a late transmission for it is dropped.
	tm, cmd = tm.Update(key("h"))
	if got := rawOf(t, cmd); got != images.KittyDelete(16) {
		t.Errorf("leaving sent %q", got)
	}
	if _, cmd := tm.Update(kittyDataMsg{16, "late"}); cmd != nil {
		t.Error("late transmission sent")
	}
	// The next image gets a new id.
	m = tm.(Model)
	m.follow(1)
	if m.kitty.id != 17 {
		t.Errorf("next id %d", m.kitty.id)
	}
}

// Terminals without placeholders, like WezTerm, get the image placed at a
// cell once the renderer has drawn the blank space for it.
func TestKittyDirect(t *testing.T) {
	m, done := kittyModel(t, "xterm-256color", "WezTerm 20240203")
	defer done()
	m, cmd := openPic(t, m)
	if m.kitty.virtual {
		t.Fatal("placeholders for WezTerm")
	}
	if strings.Contains(m.viewImage(), "\U0010EEEE") || len(m.picLines) != 4 {
		t.Errorf("viewer:\n%s", m.viewImage())
	}
	// The data goes out now, and the placement after placeDelay.
	var data string
	var tick placeTick
	for _, msg := range cmds(cmd) {
		switch v := msg.(type) {
		case tea.RawMsg:
			data = fmt.Sprint(v.Msg)
		case placeTick:
			tick = v
		}
	}
	if !strings.Contains(data, "a=t,f=100") || strings.Contains(data, "a=p") {
		t.Errorf("data %q", data[:min(len(data), 60)])
	}
	var tm tea.Model = m
	tm, cmd = tm.Update(tick)
	// 96 by 64 is 12 by 4 cells, centered in 58 columns, under 4 lines.
	if got, want := rawOf(t, cmd), images.KittyPlaceAt(16, 12, 4, 25, 5); got != want {
		t.Errorf("placed %q, want %q", got, want)
	}
	// An update that changes nothing sends nothing.
	if _, cmd := tm.Update(renderedMsg{}); cmd != nil {
		t.Error("an idle update sent kitty output")
	}
	// Help hides the image and keeps its data. Coming back places it again.
	tm, cmd = tm.Update(key("?"))
	if got := rawOf(t, cmd); got != images.KittyHide(16) {
		t.Errorf("help sent %q", got)
	}
	tm, cmd = tm.Update(key("x"))
	stale, ok := cmd().(placeTick)
	if !ok {
		t.Fatal("no placement after help")
	}
	// A resize before the tick replaces it with a newer one.
	tm, cmd = tm.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	if _, c := tm.Update(stale); c != nil {
		t.Error("a stale tick placed the image")
	}
	var newest placeTick
	for _, msg := range cmds(cmd) {
		if p, ok := msg.(placeTick); ok {
			newest = p
		}
	}
	tm, cmd = tm.Update(newest)
	if got, want := rawOf(t, cmd), images.KittyPlaceAt(16, 12, 4, 15, 5); got != want {
		t.Errorf("after resize placed %q, want %q", got, want)
	}
	tm, cmd = tm.Update(key("h"))
	if got := rawOf(t, cmd); got != images.KittyDelete(16) {
		t.Errorf("leaving sent %q", got)
	}
}

func kittyOpts(id int) kitty.Options {
	return kitty.Options{ID: id}
}

func menuSite() *content.Site {
	return &content.Site{
		Title:       "Site",
		Description: "About things",
		Logo:        []string{" /\\", "/__\\", "LOGO"},
		Entries: []*content.Entry{
			{URL: "https://x.example/p1", Title: "Post one", Kind: content.Post, Body: "p1"},
			{URL: "https://x.example/p2", Title: "Post two", Kind: content.Post, Body: "p2"},
			{URL: "https://x.example/about", Title: "About", Kind: content.Page, Body: "about"},
		},
		Nav: []content.Nav{
			{Label: "Posts", Kind: content.NavPosts},
			{Label: "Pages", Kind: content.NavPages},
			{Label: "About", Kind: content.NavPage, URL: "https://x.example/about"},
			{Label: "GitHub", Kind: content.NavLink, URL: "https://github.com/x"},
			{Label: "Gone", Kind: content.NavPage, URL: "https://x.example/gone"},
		},
	}
}

func TestMenu(t *testing.T) {
	m := Model{st: newStyles(true, ""), width: 80, height: 30, vp: viewport.New(), site: menuSite()}
	m.pickView(true)
	m.applyFilter()
	if m.view != content.NavPosts || m.navSel != 0 || len(m.visible) != 2 {
		t.Fatalf("starts on view %v, item %d, %d visible", m.view, m.navSel, len(m.visible))
	}
	var tm tea.Model = m
	tm, _ = tm.(Model).Update(key("2"))
	if got := tm.(Model); got.navSel != 1 || len(got.visible) != 1 || got.visible[0].Title != "About" {
		t.Errorf("after 2: item %d, %d visible", got.navSel, len(got.visible))
	}
	tm, _ = tm.(Model).Update(key("4"))
	if got := tm.(Model); !strings.Contains(got.status, "https://github.com/x") || !strings.Contains(got.viewList(), "github.com/x") {
		t.Errorf("link status = %q", got.status)
	}
	tm, _ = tm.(Model).Update(key("j"))
	if got := tm.(Model); got.status != "" {
		t.Errorf("status kept after a key: %q", got.status)
	}
	tm, _ = tm.(Model).Update(key("5"))
	if got := tm.(Model); got.screen != screenList || !strings.Contains(got.status, "Gone") {
		t.Errorf("missing page: screen %v, status %q", got.screen, got.status)
	}
	tm, _ = tm.(Model).Update(key("9"))
	tm, _ = tm.(Model).Update(key("3"))
	if got := tm.(Model); got.screen != screenReader || got.entry == nil || got.entry.Title != "About" {
		t.Errorf("3 did not open About: screen %v", got.screen)
	}

	// A refresh that moves the menu around keeps the same filter.
	got := tm.(Model)
	site := menuSite()
	site.Nav = []content.Nav{site.Nav[3], site.Nav[1]}
	got.site = site
	got.pickView(false)
	if got.view != content.NavPages || got.navSel != 1 {
		t.Errorf("after refresh: view %v, item %d", got.view, got.navSel)
	}
	// One that drops the filter falls back to the first list item.
	site.Nav = []content.Nav{site.Nav[0]}
	got.pickView(false)
	if got.view != content.NavAll || got.navSel != -1 {
		t.Errorf("after the menu went away: view %v, item %d", got.view, got.navSel)
	}
}

func TestHeaderWithLogo(t *testing.T) {
	m := Model{st: newStyles(true, "#e8590c"), width: 80, height: 30, site: menuSite()}
	m.pickView(true)
	m.applyFilter()
	// Blank, three logo lines, blank, title, description, menu, blank.
	if h := m.headerHeight(); h != 9 {
		t.Errorf("header height = %d", h)
	}
	lines := strings.Split(m.viewList(), "\n")
	if !strings.Contains(lines[1], "/\\") || !strings.Contains(lines[6], "About things") || !strings.Contains(lines[7], "Pages") {
		t.Errorf("header:\n%s", strings.Join(lines[:9], "\n"))
	}
	if !strings.Contains(lines[m.headerHeight()], "Post one") || m.rowAt(m.headerHeight()) != 0 {
		t.Errorf("first row %q, rowAt %d", lines[m.headerHeight()], m.rowAt(m.headerHeight()))
	}
	// Short and narrow terminals leave the logo out.
	m.height = 12
	if m.showLogo() || m.headerHeight() != 5 {
		t.Errorf("short: logo %v, header %d", m.showLogo(), m.headerHeight())
	}
	m.height, m.width = 30, 4
	if m.showLogo() {
		t.Error("logo shown wider than the terminal")
	}
}

func TestEmptyListSaysWhy(t *testing.T) {
	site := menuSite()
	site.Entries = site.Entries[2:]
	m := Model{st: newStyles(true, ""), width: 80, height: 30, site: site}
	m.pickView(true)
	m.applyFilter()
	if !strings.Contains(m.viewList(), "No posts yet.") {
		t.Errorf("empty posts view:\n%s", m.viewList())
	}
}
