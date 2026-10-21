package ui

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/burraksumer/sshr/internal/images"
)

// The image viewer uses the kitty graphics protocol in terminals that answer
// its query, and half blocks everywhere else. See the images package for the
// two ways it places an image.

// kittyState is what the viewer has told the terminal about its image.
type kittyState struct {
	on         bool   // the terminal answered the query
	term, name string // TERM and the XTVERSION name, to tell placeholders apart

	id      int  // the viewer's image in the terminal, or 0
	lastID  int  // the last id handed out
	virtual bool // the image shows through Unicode placeholders
	sent    bool // its data went out

	// Where the image goes, from drawPic: cells, and its first column.
	cols, rows, x int

	placed  placement // what the terminal shows now
	pending placement // a direct placement waiting on placeTick
	gen     int       // matches the newest placeTick
}

// placement is an image's cells and first column. The zero value means it
// isn't shown.
type placement struct{ cols, rows, x int }

// picTop is the number of lines above the image in the viewer.
const picTop = 4

// placeDelay gives the renderer time to draw the blank cells before a
// direct placement goes over them. Some terminals tie images to cells, and
// text drawn after would erase the image.
const placeDelay = 80 * time.Millisecond

// kittyDataMsg carries an encoded image for the terminal.
type kittyDataMsg struct {
	id   int
	data string
}

// placeTick asks for a direct placement once the renderer has caught up.
type placeTick struct{ gen int }

// kittyInit asks the terminal whether it supports the graphics protocol,
// and for its name. Terminals that don't know the query ignore it.
func kittyInit() tea.Cmd {
	return tea.Batch(tea.Raw(images.KittyQuery), tea.RequestTerminalVersion)
}

// newPicID hands out an id for a new image. Ids start at 16, because the
// placeholders name the image with a 256-color index, and a renderer may
// rewrite 0 to 15 as basic colors. Each image gets a new id, so a late
// transmission can't land on the next image.
func (k *kittyState) newPicID() int {
	k.lastID++
	if k.lastID < 16 || k.lastID > 255 {
		k.lastID = 16
	}
	return k.lastID
}

// open starts a new image in the viewer.
func (k *kittyState) open() {
	k.id, k.sent, k.placed, k.pending, k.cols, k.rows, k.x = 0, false, placement{}, placement{}, 0, 0, 0
	if k.on {
		k.id = k.newPicID()
		k.virtual = images.KittyPlaceholders(k.term, k.name)
	}
}

// close frees the image in the terminal.
func (k *kittyState) close() tea.Cmd {
	if k.id == 0 {
		return nil
	}
	cmd := tea.Raw(images.KittyDelete(k.id))
	k.id, k.sent, k.placed, k.pending = 0, false, placement{}, placement{}
	return cmd
}

// transmitted handles the encoded image. With placeholders, the virtual
// placement goes out with the data, since it has to follow it.
func (k *kittyState) transmitted(msg kittyDataMsg, showing bool) tea.Cmd {
	// The viewer may have closed or moved on while the PNG was encoded.
	if msg.id != k.id || k.sent || msg.data == "" {
		return nil
	}
	k.sent = true
	if k.virtual && showing {
		k.placed = placement{k.cols, k.rows, k.x}
		return tea.Raw(msg.data + images.KittyPlace(k.id, k.cols, k.rows))
	}
	return tea.Raw(msg.data)
}

// resized notes that the renderer may have cleared the screen, which takes
// direct placements with it.
func (k *kittyState) resized() {
	if !k.virtual {
		k.placed = placement{}
	}
}

// sync brings the terminal in line with the viewer: the image is placed
// where drawPic put it while the viewer shows, and hidden otherwise.
func (k *kittyState) sync(showing bool) tea.Cmd {
	if k.id == 0 || !k.sent {
		return nil
	}
	var want placement
	if showing {
		want = placement{k.cols, k.rows, k.x}
	}
	if k.virtual {
		// Placeholders only appear in the viewer, so a placement can
		// stay while it's hidden.
		if want == (placement{}) || want == k.placed {
			return nil
		}
		k.placed = want
		return tea.Raw(images.KittyPlace(k.id, want.cols, want.rows))
	}
	switch {
	case want == k.placed:
		k.pending = placement{}
		return nil
	case want == (placement{}):
		k.placed, k.pending = placement{}, placement{}
		return tea.Raw(images.KittyHide(k.id))
	case want == k.pending:
		return nil
	}
	k.pending = want
	k.gen++
	gen := k.gen
	return tea.Tick(placeDelay, func(time.Time) tea.Msg { return placeTick{gen} })
}

// place makes a direct placement that sync scheduled.
func (k *kittyState) place(msg placeTick, showing bool) tea.Cmd {
	p := k.pending
	if msg.gen != k.gen || p == (placement{}) || !showing || k.id == 0 {
		return nil
	}
	k.placed, k.pending = p, placement{}
	return tea.Raw(images.KittyPlaceAt(k.id, p.cols, p.rows, p.x, picTop+1))
}
