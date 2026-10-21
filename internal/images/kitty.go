package images

import (
	"bytes"
	"encoding/base64"
	"image/png"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
)

// The kitty graphics protocol shows real images in kitty and Ghostty. sshr
// uses its Unicode placeholders: the image is sent once and placed
// virtually, and the view draws placeholder characters where it goes. The
// placeholders are text, so the TUI renderer moves and redraws them like any
// other cell, and the terminal paints the image over them.
//
// See https://sw.kovidgoyal.net/kitty/graphics-protocol/#unicode-placeholders

// KittyQueryID is the image id of KittyQuery. A terminal that supports the
// protocol answers it with "OK".
const KittyQueryID = 31

// KittyQuery asks the terminal whether it supports the graphics protocol,
// with a one-pixel image it doesn't keep.
var KittyQuery = ansi.KittyGraphics([]byte("AAAA"), "i="+strconv.Itoa(KittyQueryID), "s=1", "v=1", "a=q", "t=d", "f=24")

// Assumed cell size in pixels, used to show an image at about its natural
// size. The terminal keeps the image's shape inside the cells it gets, so a
// wrong guess only changes the margins.
const cellW, cellH = 8, 16

// KittySize returns the cells img takes in a window of cols by rows: its
// natural size if it fits, and the window otherwise.
func KittySize(img *Image, cols, rows int) (int, int) {
	if img == nil || cols < 1 || rows < 1 {
		return 0, 0
	}
	cols, rows = min(cols, len(kittyDiacritics)), min(rows, len(kittyDiacritics))
	w, h := fit(img.Width, img.Height, cols*cellW, rows*cellH, 1)
	return min(max((w+cellW-1)/cellW, 1), cols), min(max((h+cellH-1)/cellH, 1), rows)
}

// KittyTransmit sends img to the terminal as image id, in chunks, with
// responses turned off. It encodes a PNG, so callers run it off the event
// loop. KittyPlace then shows it.
func KittyTransmit(id int, img *Image) string {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img.Pix); err != nil {
		return ""
	}
	data := base64.StdEncoding.EncodeToString(buf.Bytes())
	var b strings.Builder
	for first := true; first || data != ""; first = false {
		n := min(len(data), kitty.MaxChunkSize)
		chunk := data[:n]
		data = data[n:]
		more := "m=1"
		if data == "" {
			more = "m=0"
		}
		if first {
			b.WriteString(ansi.KittyGraphics([]byte(chunk), "a=t", "f=100", "t=d", "i="+strconv.Itoa(id), "q=2", more))
		} else {
			b.WriteString(ansi.KittyGraphics([]byte(chunk), "q=2", more))
		}
	}
	return b.String()
}

// KittyPlace places image id virtually over cols by rows cells, replacing
// its earlier placement.
func KittyPlace(id, cols, rows int) string {
	return ansi.KittyGraphics(nil, "a=p", "U=1", "i="+strconv.Itoa(id), "p=1", "c="+strconv.Itoa(cols), "r="+strconv.Itoa(rows), "q=2")
}

// KittyDelete frees image id and its placements in the terminal.
func KittyDelete(id int) string {
	return ansi.KittyGraphics(nil, "a=d", "d=I", "i="+strconv.Itoa(id), "q=2")
}

// KittyCells returns the placeholder lines for image id over cols by rows
// cells. Each cell names its row and column with diacritics, and the
// foreground color names the image. The id is sent as a 256-color index of
// at least 16, which the renderer passes through unchanged on any color
// terminal.
func KittyCells(id, cols, rows int) []string {
	cols, rows = min(cols, len(kittyDiacritics)), min(rows, len(kittyDiacritics))
	lines := make([]string, rows)
	for r := range rows {
		var b strings.Builder
		b.WriteString("\x1b[38;5;" + strconv.Itoa(id) + "m")
		for c := range cols {
			b.WriteRune(kitty.Placeholder)
			b.WriteRune(kittyDiacritics[r])
			b.WriteRune(kittyDiacritics[c])
		}
		b.WriteString("\x1b[39m")
		lines[r] = b.String()
	}
	return lines
}

// kittyDiacritics lists the row and column marks kitty defines.
var kittyDiacritics = func() []rune {
	var out []rune
	for i := 0; ; i++ {
		r := kitty.Diacritic(i)
		if i > 0 && r == kitty.Diacritic(0) {
			return out
		}
		out = append(out, r)
	}
}()
