// Package ui is the terminal reader: a list of everything on the site, a
// reader for one entry, and a help screen. The same model runs over SSH and
// in a local terminal.
package ui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/burraksumer/sshr/internal/content"
	"github.com/burraksumer/sshr/internal/render"
	"github.com/charmbracelet/x/ansi"
)

// maxReadWidth keeps lines short enough to read comfortably on wide terminals.
const maxReadWidth = 100

type screen int

const (
	screenList screen = iota
	screenReader
	screenHelp
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
	entry   *content.Entry
	doc     render.Document
	vp      viewport.Model
	digits  string
	history []*content.Entry
	status  string
}

// New returns a model that reads from store.
func New(store *content.Store) Model {
	ti := textinput.New()
	ti.Prompt = "/"
	ti.Placeholder = "search titles and tags"
	// ctrl+v reads the clipboard of the machine sshr runs on, which over SSH
	// is the server. Terminal paste still works through bracketed paste.
	ti.KeyMap.Paste.SetEnabled(false)
	m := Model{store: store, dark: true, width: 80, height: 24, filter: ti}
	m.st = newStyles(m.dark)
	m.vp = viewport.New()
	m.syncSite()
	return m
}

type pollMsg struct{}

// The store replaces its snapshot after each refresh. Sessions check for a
// new one this often, which is a pointer comparison.
const pollInterval = 2 * time.Second

func poll() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg { return pollMsg{} })
}

func (m Model) Init() tea.Cmd {
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
		selected = m.visible[m.cursor].URL
	}
	m.site = site
	m.applyFilter()
	for i, e := range m.visible {
		if selected != "" && e.URL == selected {
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

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.clampOffset()
		if m.entry != nil {
			m.renderEntry(false)
		}
		return m, nil

	case tea.BackgroundColorMsg:
		if dark := msg.IsDark(); dark != m.dark {
			m.dark = dark
			m.st = newStyles(dark)
			if m.entry != nil {
				m.renderEntry(false)
			}
		}
		return m, nil

	case pollMsg:
		m.syncSite()
		return m, poll()

	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		switch m.screen {
		case screenHelp:
			m.screen = m.prev
			return m, nil
		case screenReader:
			return m.updateReader(msg)
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

	case tea.MouseClickMsg:
		if m.screen == screenList && msg.Button == tea.MouseLeft {
			if i := m.rowAt(msg.Y); i >= 0 {
				m.cursor = i
				m.open(m.visible[i], false)
			}
		}
		return m, nil
	}
	return m, nil
}

func (m Model) updateList(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
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
			m.open(m.visible[m.cursor], false)
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
			m.follow(n)
			return m, nil
		}
	case "esc":
		if m.digits != "" || m.status != "" {
			m.digits, m.status = "", ""
			return m, nil
		}
		m.back()
		return m, nil
	case "q", "h", "left", "backspace":
		m.back()
		return m, nil
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

func (m *Model) follow(n int) {
	if n < 1 || n > len(m.doc.Links) {
		m.status = fmt.Sprintf("No link [%d]", n)
		return
	}
	link := m.doc.Links[n-1]
	if e := m.site.Lookup(link.URL); e != nil {
		m.open(e, true)
		return
	}
	m.status = "Link [" + strconv.Itoa(n) + "] " + ansi.SetHyperlink(link.URL) + link.URL + ansi.ResetHyperlink()
}

func (m *Model) open(e *content.Entry, push bool) {
	if push && m.entry != nil {
		m.history = append(m.history, m.entry)
	}
	m.entry = e
	m.screen = screenReader
	m.digits, m.status = "", ""
	m.renderEntry(true)
}

func (m *Model) back() {
	m.digits, m.status = "", ""
	if n := len(m.history); n > 0 {
		m.entry = m.history[n-1]
		m.history = m.history[:n-1]
		m.renderEntry(true)
		return
	}
	m.entry = nil
	m.screen = screenList
	m.syncSite()
}

func (m *Model) renderEntry(top bool) {
	w := min(m.width, maxReadWidth)
	m.vp.SetWidth(m.width)
	m.vp.SetHeight(max(m.height-1, 1))

	doc, err := render.Markdown(m.entry.Body, render.Options{Width: w, Dark: m.dark, BaseURL: m.entry.URL})
	if err != nil {
		doc = render.Document{Content: m.entry.Body}
	}
	m.doc = doc

	var b strings.Builder
	pad := "  "
	b.WriteString("\n" + pad + m.st.title.Render(ansi.Truncate(m.entry.Title, max(w-4, 10), "…")) + "\n")
	if meta := entryMeta(m.entry); meta != "" {
		b.WriteString(pad + m.st.dim.Render(meta) + "\n")
	}
	b.WriteString(doc.Content)
	b.WriteString("\n")

	// Rewrapping changes the line count, so keep the reading position as a
	// fraction of the document rather than a line number.
	pct := m.vp.ScrollPercent()
	m.vp.SetContent(b.String())
	if top {
		m.vp.GotoTop()
	} else {
		m.vp.SetYOffset(int(pct * float64(max(m.vp.TotalLineCount()-m.vp.Height(), 0))))
	}
}

func entryMeta(e *content.Entry) string {
	var parts []string
	if !e.Date.IsZero() {
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
	gap := max(m.width-ansi.StringWidth(left)-ansi.StringWidth(right)-2, 1)
	return m.vp.View() + "\n " + left + strings.Repeat(" ", gap) + right
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
		{"3 enter", "follow link [3]"},
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
