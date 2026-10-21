package ui

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/viewport"

	tea "charm.land/bubbletea/v2"
	"github.com/burraksumer/sshr/internal/content"
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
	m := New(content.NewStore(&content.Discoverer{}, nil))
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
