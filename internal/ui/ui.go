// Package ui is the terminal reader. It has a list of everything on the site, a
// reader for one entry, an image viewer and a help screen. The same model runs over SSH and
// in a local terminal.
package ui

import (
	"context"
	"fmt"
	"image/color"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/burraksumer/sshr/internal/content"
	"github.com/burraksumer/sshr/internal/images"
	"github.com/burraksumer/sshr/internal/render"
	"github.com/burraksumer/sshr/internal/sanitize"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/time/rate"
)

// maxReadWidth keeps lines short enough to read comfortably on wide terminals.
const maxReadWidth = 100

type screen int

const (
	screenList screen = iota
	screenReader
	screenHelp
	screenImage
)

// Model is the Bubble Tea model for one visitor.
type Model struct {
	store   *content.Store
	site    *content.Site
	loadErr error

	width, height int
	dark          bool
	st            styles
	screen        screen
	prev          screen // screen to return to from help

	// list
	cursor, offset int
	filter         textinput.Model
	filtering      bool
	visible        []*content.Entry

	// reader
	resizeGen  int
	pending    bool          // a render is running in the background
	pendingPos float64       // where it will open
	renders    *rate.Limiter // re-renders after resizes
	resizing   bool          // a resize is waiting to settle
	keepPos    float64       // reading position from before it started
	entry      *content.Entry
	doc        render.Document
	vp         viewport.Model
	digits     string
	history    []visit
	status     string

	// image viewer
	images   *images.Loader // nil when images are off
	pic      render.Link
	picImg   *images.Image
	picErr   error
	picLines []string

	kitty kittyState
}

// New returns a model that reads from store and loads images with loader,
// which may be nil to leave images out.
func New(store *content.Store, loader *images.Loader) Model {
	ti := textinput.New()
	ti.Prompt = "/"
	ti.Placeholder = "search titles, summaries and tags"
	// ctrl+v reads the clipboard of the machine sshr runs on, which over SSH
	// is the server. Terminal paste still works through bracketed paste.
	ti.KeyMap.Paste.SetEnabled(false)
	m := Model{store: store, images: loader, dark: true, width: 80, height: 24, filter: ti, renders: rate.NewLimiter(2, 3)}
	m.st = newStyles(m.dark)
	m.vp = viewport.New()
	m.syncSite()
	return m
}

// resizeSettle is how long the window size must stay the same before the
// entry is rendered again.
const resizeSettle = 120 * time.Millisecond

type resizedMsg struct{ gen int }

// visit is an entry in the back history and where the reader was in it.
type visit struct {
	entry *content.Entry
	pos   float64
}

// renderKey names the render the view is waiting for.
type renderKey struct {
	id    any
	width int
	dark  bool
}

// renderedMsg brings back a render done off the event loop.
type renderedMsg struct {
	key renderKey
	doc render.Document
	pos float64
}

// syncRender is the body size under which rendering is quick enough to do
// inside Update. Larger bodies render in the background so keys keep
// working.
const syncRender = 64 << 10

type pollMsg struct{}

// The store replaces its snapshot after each refresh. Sessions check for a
// new one this often, which is a pointer comparison.
const pollInterval = 2 * time.Second

func poll() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg { return pollMsg{} })
}

func (m Model) Init() tea.Cmd {
	if m.images != nil {
		return tea.Batch(tea.RequestBackgroundColor, poll(), kittyInit())
	}
	return tea.Batch(tea.RequestBackgroundColor, poll())
}

// syncSite picks up a newer snapshot from the store, if there is one, and
// keeps the cursor on the same entry.
func (m *Model) syncSite() {
	site, err := m.store.Site()
	m.loadErr = err
	if site == nil || site == m.site {
		return
	}
	var selected string
	if m.cursor < len(m.visible) {
		selected = m.visible[m.cursor].Key()
	}
	m.site = site
	m.applyFilter()
	for i, e := range m.visible {
		if selected != "" && e.Key() == selected {
			m.cursor = i
			m.clampOffset()
			break
		}
	}
}

func (m *Model) applyFilter() {
	if m.site == nil {
		m.visible = nil
		return
	}
	q := strings.ToLower(strings.TrimSpace(m.filter.Value()))
	m.visible = m.visible[:0]
	for _, e := range m.site.Entries {
		if q == "" || matches(e, q) {
			m.visible = append(m.visible, e)
		}
	}
	m.cursor = min(m.cursor, max(len(m.visible)-1, 0))
	m.clampOffset()
}

func matches(e *content.Entry, q string) bool {
	if strings.Contains(strings.ToLower(e.Title), q) || strings.Contains(strings.ToLower(e.Summary), q) {
		return true
	}
	for _, t := range e.Tags {
		if strings.Contains(strings.ToLower(t), q) {
			return true
		}
	}
	return false
}

// Update handles msg, then brings the terminal's image in line with the
// screen.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	nm := next.(Model)
	if sync := nm.kitty.sync(nm.screen == screenImage); sync != nil {
		cmd = tea.Batch(cmd, sync)
	}
	return nm, cmd
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.clampOffset()
		m.kitty.resized()
		m.drawPic()
		if m.entry == nil {
			return m, nil
		}
		// Dragging a window edge sends many sizes. Resize the viewport now
		// and render the text once the size settles, at the position the
		// reader was at before the first of them.
		if !m.resizing {
			m.keepPos, m.resizing = m.position(), true
		}
		m.vp.SetWidth(m.width)
		m.vp.SetHeight(max(m.height-1, 1))
		m.resizeGen++
		gen := m.resizeGen
		return m, tea.Tick(resizeSettle, func(time.Time) tea.Msg { return resizedMsg{gen} })

	case resizedMsg:
		if msg.gen != m.resizeGen || m.entry == nil {
			return m, nil
		}
		// A client changing its window size nonstop gets a few renders a
		// second, not one per change.
		if m.renders != nil && !m.renders.Allow() {
			gen := m.resizeGen
			return m, tea.Tick(resizeSettle, func(time.Time) tea.Msg { return resizedMsg{gen} })
		}
		pos := m.keepPos
		m.resizing = false
		return m, m.renderEntry(pos)

	case imageMsg:
		if m.screen == screenImage && msg.url == m.pic.URL && m.picImg == nil && m.picErr == nil {
			m.picImg, m.picErr = msg.img, msg.err
			m.drawPic()
			if m.kitty.id != 0 && m.picImg != nil {
				id, img := m.kitty.id, m.picImg
				return m, func() tea.Msg { return kittyDataMsg{id, images.KittyTransmit(id, img)} }
			}
		}
		return m, nil

	case kittyDataMsg:
		return m, m.kitty.transmitted(msg, m.screen == screenImage)

	case placeTick:
		return m, m.kitty.place(msg, m.screen == screenImage)

	case tea.EnvMsg:
		m.kitty.term = msg.Getenv("TERM")
		return m, nil

	case tea.TerminalVersionMsg:
		m.kitty.name = msg.Name
		return m, nil

	case uv.KittyGraphicsEvent:
		if msg.Options.ID == images.KittyQueryID && string(msg.Payload) == "OK" {
			m.kitty.on = true
		}
		return m, nil

	case renderedMsg:
		if m.entry != nil && msg.key == m.currentKey() {
			m.show(msg.doc, msg.pos)
		}
		return m, nil

	case tea.BackgroundColorMsg:
		if dark := msg.IsDark(); dark != m.dark {
			m.dark = dark
			m.st = newStyles(dark)
			m.drawPic()
			if m.entry != nil {
				return m, m.renderEntry(m.position())
			}
		}
		return m, nil

	case pollMsg:
		m.syncSite()
		return m, poll()

	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Sequence(m.closePic(), tea.Quit)
		}
		switch m.screen {
		case screenHelp:
			m.screen = m.prev
			// The window may have changed size behind the help screen.
			m.drawPic()
			return m, nil
		case screenReader:
			return m.updateReader(msg)
		case screenImage:
			switch msg.String() {
			case "esc", "q", "h", "left", "backspace":
				cmd := m.closePic()
				m.screen = screenReader
				return m, cmd
			case "?":
				m.prev, m.screen = m.screen, screenHelp
			}
			return m, nil
		default:
			return m.updateList(msg)
		}

	case tea.MouseWheelMsg:
		if m.screen == screenList {
			if msg.Button == tea.MouseWheelUp {
				m.move(-1)
			} else if msg.Button == tea.MouseWheelDown {
				m.move(1)
			}
			return m, nil
		}
		if m.screen == screenReader {
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		}

	case tea.PasteMsg:
		// Terminal paste into the search box.
		if m.screen == screenList && m.filtering {
			var cmd tea.Cmd
			m.filter, cmd = m.filter.Update(msg)
			m.applyFilter()
			return m, cmd
		}
		return m, nil

	case tea.MouseClickMsg:
		if m.screen == screenList && msg.Button == tea.MouseLeft {
			if i := m.rowAt(msg.Y); i >= 0 {
				m.cursor = i
				return m, m.open(m.visible[i], false)
			}
		}
		return m, nil
	}
	return m, nil
}

func (m Model) updateList(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	// The loading screen only offers quitting.
	if m.site == nil {
		if k == "q" {
			return m, tea.Quit
		}
		return m, nil
	}
	if m.filtering {
		switch k {
		case "esc":
			m.filtering = false
			m.filter.Blur()
			m.filter.SetValue("")
			m.applyFilter()
			return m, nil
		case "enter":
			m.filtering = false
			m.filter.Blur()
			return m, nil
		case "up", "down", "ctrl+p", "ctrl+n":
			if k == "up" || k == "ctrl+p" {
				m.move(-1)
			} else {
				m.move(1)
			}
			return m, nil
		}
		var cmd tea.Cmd
		m.filter, cmd = m.filter.Update(msg)
		m.applyFilter()
		return m, cmd
	}

	switch k {
	case "q":
		return m, tea.Quit
	case "esc":
		if m.filter.Value() != "" {
			m.filter.SetValue("")
			m.applyFilter()
		}
	case "j", "down", "ctrl+n":
		m.move(1)
	case "k", "up", "ctrl+p":
		m.move(-1)
	case "g", "home":
		m.move(-len(m.visible))
	case "G", "end":
		m.move(len(m.visible))
	case "ctrl+d", "pgdown", "space", "f":
		m.move(max(m.listHeight()/2, 1))
	case "ctrl+u", "pgup", "b":
		m.move(-max(m.listHeight()/2, 1))
	case "enter", "l", "right":
		if m.cursor < len(m.visible) {
			return m, m.open(m.visible[m.cursor], false)
		}
	case "/":
		m.filtering = true
		return m, m.filter.Focus()
	case "?":
		m.prev, m.screen = m.screen, screenHelp
	case "r":
		m.syncSite()
	}
	return m, nil
}

func (m Model) updateReader(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if len(k) == 1 && k[0] >= '0' && k[0] <= '9' {
		if len(m.digits) < 4 {
			m.digits += k
		}
		m.status = ""
		return m, nil
	}
	switch k {
	case "enter":
		if m.digits != "" {
			n, _ := strconv.Atoi(m.digits)
			m.digits = ""
			return m, m.follow(n)
		}
	case "esc":
		if m.digits != "" || m.status != "" {
			m.digits, m.status = "", ""
			return m, nil
		}
		return m, m.back()
	case "backspace":
		if m.digits != "" {
			m.digits = m.digits[:len(m.digits)-1]
			return m, nil
		}
		return m, m.back()
	case "q", "h", "left":
		return m, m.back()
	case "g", "home":
		m.vp.GotoTop()
		return m, nil
	case "G", "end":
		m.vp.GotoBottom()
		return m, nil
	case "?":
		m.prev, m.screen = m.screen, screenHelp
		return m, nil
	}
	m.digits = ""
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func (m *Model) follow(n int) tea.Cmd {
	if n < 1 || n > len(m.doc.Links) {
		m.status = fmt.Sprintf("No link [%d]", n)
		return nil
	}
	link := m.doc.Links[n-1]
	if link.Image && m.images != nil {
		return m.viewPic(link)
	}
	if e := m.site.Lookup(link.URL); e != nil {
		return m.open(e, true)
	}
	m.status = "Link [" + strconv.Itoa(n) + "] " + link.URL
	if render.Clickable(link.URL) {
		m.status = "Link [" + strconv.Itoa(n) + "] " + ansi.SetHyperlink(link.URL) + link.URL + ansi.ResetHyperlink()
	}
	return nil
}

// imageMsg brings back an image loaded off the event loop.
type imageMsg struct {
	url string
	img *images.Image
	err error
}

// imageWait is how long the viewer waits for an image. The loader keeps
// going after that, so a later look may find it cached.
const imageWait = 40 * time.Second

// viewPic opens the image viewer on link and starts loading it.
func (m *Model) viewPic(link render.Link) tea.Cmd {
	m.screen, m.pic, m.picImg, m.picErr, m.picLines = screenImage, link, nil, nil, nil
	m.kitty.open()
	loader, u := m.images, link.URL
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), imageWait)
		defer cancel()
		img, err := loader.Load(ctx, u)
		return imageMsg{u, img, err}
	}
}

// closePic leaves the viewer's image, freeing it in the terminal.
func (m *Model) closePic() tea.Cmd {
	m.picImg, m.picErr, m.picLines = nil, nil, nil
	return m.kitty.close()
}

// drawPic lays out the viewer's image for the current window size: half
// blocks, kitty placeholders, or blank cells for a direct kitty placement.
func (m *Model) drawPic() {
	if m.screen != screenImage || m.picImg == nil {
		return
	}
	cols, rows := max(m.width-2, 1), max(m.height-picChrome, 1)
	if k := &m.kitty; k.id != 0 {
		c, r := images.KittySize(m.picImg, cols, rows)
		pad := (cols - c) / 2
		// The view's margin is one column, and columns count from 1.
		k.cols, k.rows, k.x = c, r, 2+pad
		m.picLines = make([]string, r)
		if k.virtual {
			for i, l := range images.KittyCells(k.id, c, r) {
				m.picLines[i] = strings.Repeat(" ", pad) + l
			}
		}
		return
	}
	bg := color.RGBA{0, 0, 0, 255}
	if !m.dark {
		bg = color.RGBA{255, 255, 255, 255}
	}
	m.picLines = images.Draw(m.picImg, cols, rows, bg)
}

// picChrome is the number of lines around the image in the viewer: a blank
// line, the title, the URL and a blank line above it, and a blank line and
// the footer below.
const picChrome = 6

// leave forgets a resize in progress, so its position isn't applied to
// the next entry or kept after going back to the list.
func (m *Model) leave() {
	m.resizing = false
	m.resizeGen++
}

func (m *Model) open(e *content.Entry, push bool) tea.Cmd {
	// A click can open an entry while the search box has focus.
	if m.filtering {
		m.filtering = false
		m.filter.Blur()
	}
	if push && m.entry != nil {
		m.history = append(m.history, visit{m.entry, m.position()})
	}
	m.leave()
	m.entry = e
	m.screen = screenReader
	m.digits, m.status = "", ""
	return m.renderEntry(0)
}

func (m *Model) back() tea.Cmd {
	m.digits, m.status = "", ""
	m.leave()
	if n := len(m.history); n > 0 {
		v := m.history[n-1]
		m.history = m.history[:n-1]
		m.entry = v.entry
		return m.renderEntry(v.pos)
	}
	m.entry = nil
	m.screen = screenList
	m.syncSite()
	return nil
}

func (m *Model) currentKey() renderKey {
	return renderKey{m.entry.RenderID(), min(m.width, maxReadWidth), m.dark}
}

// renderEntry shows the current entry at reading position pos, from 0 at
// the top to 1 at the bottom. A large body that isn't cached renders in
// the background, and the returned command delivers it.
func (m *Model) renderEntry(pos float64) tea.Cmd {
	m.vp.SetWidth(m.width)
	m.vp.SetHeight(max(m.height-1, 1))
	key := m.currentKey()
	opts := render.Options{Width: key.width, Dark: m.dark, BaseURL: m.entry.BaseURL()}
	if doc, ok := render.Peek(key.id, opts); ok || len(m.entry.Body) < syncRender {
		if !ok {
			doc = renderDoc(m.entry, opts)
		}
		m.show(doc, pos)
		return nil
	}
	m.doc = render.Document{}
	m.pending, m.pendingPos = true, pos
	m.vp.SetContent(m.header() + "\n  " + m.st.dim.Render("Rendering…") + "\n")
	e := m.entry
	return func() tea.Msg {
		return renderedMsg{key: key, doc: renderDoc(e, opts), pos: pos}
	}
}

func renderDoc(e *content.Entry, opts render.Options) render.Document {
	doc, err := render.Cached(e.RenderID(), e.Body, opts)
	if err != nil {
		// The body is sanitized markdown, but render failed, so show it
		// unstyled rather than nothing.
		doc = render.Document{Content: e.Body}
	}
	return doc
}

// header is the entry's title and date block above its text.
func (m *Model) header() string {
	w := min(m.width, maxReadWidth)
	var b strings.Builder
	b.WriteString("\n  " + m.st.title.Render(ansi.Truncate(m.entry.Title, max(w-4, 10), "…")) + "\n")
	if meta := entryMeta(m.entry); meta != "" {
		b.WriteString("  " + m.st.dim.Render(meta) + "\n")
	}
	return b.String()
}

// show puts a rendered document in the viewport at reading position pos.
// Rewrapping changes the line count, so the position is a fraction of the
// document rather than a line number.
func (m *Model) show(doc render.Document, pos float64) {
	m.doc, m.pending = doc, false
	m.vp.SetContent(m.header() + doc.Content + "\n")
	m.vp.SetYOffset(int(pos * float64(max(m.vp.TotalLineCount()-m.vp.Height(), 0))))
}

// position is the reading position in the current entry. While a render is
// pending, the viewport holds a placeholder, so it's where the render will
// open.
func (m *Model) position() float64 {
	if m.pending {
		return m.pendingPos
	}
	return readingPosition(m.vp)
}

// readingPosition is how far down the document the viewport is, from 0 at
// the top to 1 at the bottom. Unlike ScrollPercent it is 0, not 1, when the
// whole document fits.
func readingPosition(vp viewport.Model) float64 {
	scrollable := vp.TotalLineCount() - vp.Height()
	if scrollable <= 0 {
		return 0
	}
	return float64(vp.YOffset()) / float64(scrollable)
}

func entryMeta(e *content.Entry) string {
	var parts []string
	switch {
	case e.Date.IsZero():
	case e.Kind == content.Page:
		// A page's date is when it last changed, from the sitemap.
		parts = append(parts, "Updated "+e.Date.Format("January 2, 2006"))
	default:
		parts = append(parts, e.Date.Format("January 2, 2006"))
	}
	if len(e.Tags) > 0 {
		parts = append(parts, strings.Join(e.Tags, ", "))
	}
	return strings.Join(parts, " · ")
}

func (m *Model) move(delta int) {
	if len(m.visible) == 0 {
		return
	}
	m.cursor = max(0, min(len(m.visible)-1, m.cursor+delta))
	m.clampOffset()
}

// The list starts below the header and stops above the footer.
func (m Model) headerHeight() int {
	h := 3 // blank line, title, blank line
	if m.site != nil && m.site.Description != "" {
		h++
	}
	return h
}

func (m Model) listHeight() int {
	return max(m.height-m.headerHeight()-2, 1)
}

func (m *Model) clampOffset() {
	h := m.listHeight()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+h {
		m.offset = m.cursor - h + 1
	}
	m.offset = max(0, min(m.offset, max(len(m.visible)-h, 0)))
}

func (m Model) rowAt(y int) int {
	i := y - m.headerHeight() + m.offset
	if y < m.headerHeight() || i < 0 || i >= len(m.visible) || y >= m.headerHeight()+m.listHeight() {
		return -1
	}
	return i
}

func (m Model) View() tea.View {
	var s string
	switch {
	case m.site == nil:
		s = m.viewLoading()
	case m.screen == screenHelp:
		s = m.viewHelp()
	case m.screen == screenReader:
		s = m.viewReader()
	case m.screen == screenImage:
		s = m.viewImage()
	default:
		s = m.viewList()
	}
	v := tea.NewView(s)
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	if m.site != nil {
		v.WindowTitle = m.site.Title
	}
	return v
}

func (m Model) viewLoading() string {
	msg := "Fetching the site. This takes a few seconds the first time."
	if m.loadErr != nil {
		msg = m.st.err.Render("Couldn't read the site: "+m.loadErr.Error()) + "\n  Retrying in the background."
	}
	return "\n  " + msg + "\n\n  " + m.st.dim.Render("q quit")
}

func (m Model) viewList() string {
	w := m.width
	var b strings.Builder
	b.WriteString("\n " + m.st.title.Render(ansi.Truncate(m.site.Title, max(w-2, 1), "…")) + "\n")
	if m.site.Description != "" {
		b.WriteString(" " + m.st.dim.Render(ansi.Truncate(m.site.Description, max(w-2, 1), "…")) + "\n")
	}
	b.WriteString("\n")

	h := m.listHeight()
	rightW := 12
	for i := m.offset; i < m.offset+h; i++ {
		if i >= len(m.visible) {
			b.WriteString("\n")
			continue
		}
		e := m.visible[i]
		right := "page"
		if e.Kind == content.Post {
			right = ""
			if !e.Date.IsZero() {
				right = e.Date.Format("2006-01-02")
			}
		}
		titleW := max(w-rightW-4, 8)
		title := ansi.Truncate(e.Title, titleW, "…")
		gap := max(w-2-ansi.StringWidth(title)-ansi.StringWidth(right)-2, 1)
		line := " " + title + strings.Repeat(" ", gap) + right + " "
		if i == m.cursor {
			b.WriteString(" " + m.st.selected.Render(line) + "\n")
		} else {
			b.WriteString(" " + line[:len(line)-len(right)-1] + m.st.dim.Render(right) + " \n")
		}
	}

	b.WriteString("\n")
	switch {
	case m.filtering:
		b.WriteString(" " + m.filter.View())
	case m.filter.Value() != "":
		b.WriteString(" " + m.st.accent.Render("/"+m.filter.Value()) + m.st.dim.Render(fmt.Sprintf("  %d of %d · esc clear", len(m.visible), len(m.site.Entries))))
	default:
		b.WriteString(" " + m.footer("↑↓/jk move", "enter read", "/ search", "? help", "q quit"))
	}
	return b.String()
}

func (m Model) viewReader() string {
	left := m.footer("↑↓/jk scroll", "h back", "? help")
	if len(m.doc.Links) > 0 {
		left = m.footer("↑↓/jk scroll", "1-9 enter link", "h back", "? help")
	}
	switch {
	case m.digits != "":
		left = m.st.accent.Render("link "+m.digits+"_") + m.st.dim.Render("  enter open · esc cancel")
	case m.status != "":
		left = m.status
	}
	right := m.st.dim.Render(fmt.Sprintf("%3.0f%%", m.vp.ScrollPercent()*100))
	// The percentage always fits, and the hints give way on narrow terminals.
	left = ansi.Truncate(left, max(m.width-ansi.StringWidth(right)-3, 0), "…")
	gap := max(m.width-ansi.StringWidth(left)-ansi.StringWidth(right)-2, 1)
	return m.vp.View() + "\n " + left + strings.Repeat(" ", gap) + right
}

func (m Model) viewImage() string {
	w := max(m.width-2, 1)
	title := m.pic.Alt
	if title == "" {
		title = "Image"
	}
	link := ansi.Truncate(m.pic.URL, w, "…")
	if render.Clickable(m.pic.URL) {
		link = ansi.SetHyperlink(m.pic.URL) + link + ansi.ResetHyperlink()
	}
	var b strings.Builder
	b.WriteString("\n " + m.st.title.Render(ansi.Truncate(title, w, "…")) + "\n " + m.st.dim.Render(link) + "\n\n")
	body := m.picLines
	switch {
	case m.picErr != nil:
		body = []string{m.st.err.Render(ansi.Truncate("Couldn't load the image: "+sanitize.Line(m.picErr.Error()), w, "…"))}
	case m.picImg == nil:
		body = []string{m.st.dim.Render("Loading the image…")}
	}
	rows := max(m.height-picChrome, 1)
	for i := range rows {
		if i < len(body) {
			b.WriteString(" " + body[i])
		}
		b.WriteString("\n")
	}
	info := m.footer("h back", "? help")
	if m.picImg != nil {
		info += m.st.dim.Render(fmt.Sprintf("  %d×%d %s", m.picImg.Width, m.picImg.Height, m.picImg.Format))
	}
	b.WriteString("\n " + ansi.Truncate(info, w, "…"))
	return b.String()
}

func (m Model) footer(items ...string) string {
	parts := make([]string, len(items))
	for i, it := range items {
		k, desc, _ := strings.Cut(it, " ")
		parts[i] = m.st.key.Render(k) + " " + m.st.dim.Render(desc)
	}
	return ansi.Truncate(strings.Join(parts, m.st.dim.Render(" · ")), max(m.width-2, 1), "…")
}

func (m Model) viewHelp() string {
	rows := [][2]string{
		{"", "List"},
		{"↑ ↓  j k", "move"},
		{"enter  l", "read the selected entry"},
		{"g  G", "first, last"},
		{"/", "search titles, summaries and tags"},
		{"esc", "clear search"},
		{"", ""},
		{"", "Reader"},
		{"↑ ↓  j k", "scroll"},
		{"space  b", "page down, page up"},
		{"g  G", "top, bottom"},
		{"3 enter", "follow link [3], or view it if it's an image"},
		{"h  esc  q", "back"},
		{"", ""},
		{"ctrl+c", "quit from anywhere"},
	}
	var b strings.Builder
	b.WriteString("\n " + m.st.title.Render("Keys") + "\n\n")
	for _, r := range rows {
		switch {
		case r[0] == "" && r[1] == "":
			b.WriteString("\n")
		case r[0] == "":
			b.WriteString(" " + m.st.accent.Render(r[1]) + "\n")
		default:
			b.WriteString(fmt.Sprintf("   %s %s\n", m.st.key.Render(fmt.Sprintf("%-10s", r[0])), r[1]))
		}
	}
	b.WriteString("\n " + m.st.dim.Render("any key to close"))
	return b.String()
}
