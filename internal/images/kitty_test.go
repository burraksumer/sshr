package images

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi/kitty"
)

func TestKittySize(t *testing.T) {
	small := &Image{Width: 96, Height: 64}
	if c, r := KittySize(small, 80, 40); c != 12 || r != 4 {
		t.Errorf("natural size = %d×%d", c, r)
	}
	wide := &Image{Width: 4000, Height: 1000}
	if c, r := KittySize(wide, 80, 40); c != 80 || r != 10 {
		t.Errorf("fit to width = %d×%d", c, r)
	}
	if c, r := KittySize(wide, 1000, 1000); c > len(kittyDiacritics) || r > len(kittyDiacritics) {
		t.Errorf("past the diacritics: %d×%d", c, r)
	}
	if c, r := KittySize(&Image{Width: 1, Height: 1}, 80, 40); c != 1 || r != 1 {
		t.Errorf("one pixel = %d×%d", c, r)
	}
}

func TestKittyCells(t *testing.T) {
	if len(kittyDiacritics) != 297 {
		t.Errorf("%d diacritics", len(kittyDiacritics))
	}
	lines := KittyCells(42, 3, 2)
	if len(lines) != 2 {
		t.Fatalf("%d lines", len(lines))
	}
	d := kittyDiacritics
	p := string(kitty.Placeholder)
	want := "\x1b[38;5;42m" + p + string(d[1]) + string(d[0]) + p + string(d[1]) + string(d[1]) + p + string(d[1]) + string(d[2]) + "\x1b[39m"
	if lines[1] != want {
		t.Errorf("row 1 = %q\nwant    %q", lines[1], want)
	}
}

func TestKittyTransmit(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 300, 200))
	for i := range src.Pix {
		src.Pix[i] = uint8(i * 7)
	}
	for i := 3; i < len(src.Pix); i += 4 {
		src.Pix[i] = 0xff
	}
	seq := KittyTransmit(20, &Image{Pix: src})
	parts := regexp.MustCompile("\x1b_G([^;\x1b]*)(?:;([^\x1b]*))?\x1b\\\\").FindAllStringSubmatch(seq, -1)
	if len(parts) < 2 || strings.Join(regexp.MustCompile("\x1b_G[^\x1b]*\x1b\\\\").FindAllString(seq, -1), "") != seq {
		t.Fatalf("%d chunks, or stray bytes between them", len(parts))
	}
	var data strings.Builder
	for i, p := range parts {
		switch {
		case i == 0 && p[1] != "a=t,f=100,t=d,i=20,q=2,m=1":
			t.Errorf("first chunk options %q", p[1])
		case i > 0 && i < len(parts)-1 && p[1] != "q=2,m=1":
			t.Errorf("chunk %d options %q", i, p[1])
		case i == len(parts)-1 && p[1] != "q=2,m=0":
			t.Errorf("last chunk options %q", p[1])
		}
		if len(p[2]) > kitty.MaxChunkSize {
			t.Errorf("chunk %d has %d bytes", i, len(p[2]))
		}
		data.WriteString(p[2])
	}
	raw, err := base64.StdEncoding.DecodeString(data.String())
	if err != nil {
		t.Fatal(err)
	}
	got, err := png.Decode(bytes.NewReader(raw))
	if err != nil || got.Bounds() != src.Bounds() || color.RGBAModel.Convert(got.At(10, 10)) != src.At(10, 10) {
		t.Errorf("round trip: %v %v", got.Bounds(), err)
	}
	if got := KittyPlace(20, 12, 4); got != "\x1b_Ga=p,U=1,i=20,p=1,c=12,r=4,q=2\x1b\\" {
		t.Errorf("place = %q", got)
	}
	if got := KittyDelete(20); got != "\x1b_Ga=d,d=I,i=20,q=2\x1b\\" {
		t.Errorf("delete = %q", got)
	}
}
