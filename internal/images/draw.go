package images

import (
	"image"
	"image/color"
	"strconv"
	"strings"

	xdraw "golang.org/x/image/draw"
)

// maxGrow is how much Draw may enlarge a small image.
const maxGrow = 4

// Draw renders img in at most cols by rows cells, centered horizontally. Each
// cell is an upper half block, colored with the pixel above as foreground
// and the one below as background. Mostly transparent pixels are left
// blank, and the rest are blended onto bg, a guess at the terminal's
// background. Colors are 24-bit, and the terminal program downsamples them
// for terminals with fewer colors.
func Draw(img *Image, cols, rows int, bg color.RGBA) []string {
	if img == nil || cols < 1 || rows < 1 {
		return nil
	}
	b := img.Pix.Bounds()
	w, h := fit(b.Dx(), b.Dy(), cols, rows*2, maxGrow)
	px := img.Pix
	switch {
	case w < b.Dx():
		px = shrink(img.Pix, w, h)
	case w > b.Dx():
		// Enlarging keeps pixels sharp, which suits icons and pixel art.
		px = image.NewRGBA(image.Rect(0, 0, w, h))
		xdraw.NearestNeighbor.Scale(px, px.Bounds(), img.Pix, b, xdraw.Src, nil)
	}

	pad := strings.Repeat(" ", (cols-w)/2)
	lines := make([]string, 0, (h+1)/2)
	for y := 0; y < h; y += 2 {
		var sb strings.Builder
		sb.WriteString(pad)
		var fg, bgc string
		set := func(code *string, want string) {
			if *code != want {
				sb.WriteString("\x1b[" + want + "m")
				*code = want
			}
		}
		for x := range w {
			top, topOK := pixel(px, x, y, bg)
			var bot color.RGBA
			bottomOK := false
			if y+1 < h {
				bot, bottomOK = pixel(px, x, y+1, bg)
			}
			switch {
			case topOK && bottomOK:
				set(&fg, "38;2;"+rgb(top))
				set(&bgc, "48;2;"+rgb(bot))
				sb.WriteString("▀")
			case topOK:
				set(&fg, "38;2;"+rgb(top))
				set(&bgc, "49")
				sb.WriteString("▀")
			case bottomOK:
				set(&fg, "38;2;"+rgb(bot))
				set(&bgc, "49")
				sb.WriteString("▄")
			default:
				set(&bgc, "49")
				sb.WriteString(" ")
			}
		}
		sb.WriteString("\x1b[m")
		lines = append(lines, sb.String())
	}
	return lines
}

// pixel returns the color at x, y blended onto bg, and false when the pixel
// is mostly transparent.
func pixel(img *image.RGBA, x, y int, bg color.RGBA) (color.RGBA, bool) {
	c := img.RGBAAt(x, y)
	if c.A < 0x80 {
		return c, false
	}
	// Premultiplied, so blending adds the background's share.
	k := 0xff - uint32(c.A)
	blend := func(v, b uint8) uint8 { return uint8(uint32(v) + uint32(b)*k/0xff) }
	return color.RGBA{blend(c.R, bg.R), blend(c.G, bg.G), blend(c.B, bg.B), 0xff}, true
}

func rgb(c color.RGBA) string {
	return strconv.Itoa(int(c.R)) + ";" + strconv.Itoa(int(c.G)) + ";" + strconv.Itoa(int(c.B))
}
