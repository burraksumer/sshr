package images

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/burraksumer/sshr/internal/fetch"
)

func pngOf(w, h int, c color.Color) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

// pngHeader is a PNG that claims w by h pixels and has no pixel data.
func pngHeader(w, h uint32) []byte {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 6 // 8-bit RGBA
	binary.Write(&b, binary.BigEndian, uint32(len(ihdr)))
	chunk := append([]byte("IHDR"), ihdr...)
	b.Write(chunk)
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return b.Bytes()
}

func TestDecode(t *testing.T) {
	img, err := decode(pngOf(2000, 1000, color.White))
	if err != nil {
		t.Fatal(err)
	}
	if img.Width != 2000 || img.Height != 1000 || img.Format != "png" || img.Pix.Bounds().Dx() != maxSide || img.Pix.Bounds().Dy() != maxSide/2 {
		t.Errorf("decoded %dx%d %s to %v", img.Width, img.Height, img.Format, img.Pix.Bounds())
	}
	// The size is checked from the header, before any pixels are decoded.
	if _, err := decode(pngHeader(20000, 20000)); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("huge image: %v", err)
	}
	for _, body := range []string{`<svg xmlns="http://www.w3.org/2000/svg"></svg>`, "", "GIF89a"} {
		if _, err := decode([]byte(body)); err == nil {
			t.Errorf("decoded %q", body)
		}
	}
	if _, err := decode([]byte(`<svg/>`)); !errors.Is(err, ErrUnsupported) {
		t.Errorf("svg: %v", err)
	}
}

func TestDraw(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 2, 3))
	red, blue := color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}
	src.SetRGBA(0, 0, red)
	src.SetRGBA(0, 1, blue)
	src.SetRGBA(1, 1, red) // above it is transparent
	src.SetRGBA(0, 2, blue)
	lines := Draw(&Image{Pix: src}, 2, 2, color.RGBA{})
	if len(lines) != 2 {
		t.Fatalf("%d lines", len(lines))
	}
	want0 := "\x1b[38;2;255;0;0m\x1b[48;2;0;0;255m▀\x1b[49m▄\x1b[m"
	if lines[0] != want0 {
		t.Errorf("line 0 = %q\nwant     %q", lines[0], want0)
	}
	// The odd last row has only a top half, and transparency is blank.
	if want1 := "\x1b[38;2;0;0;255m\x1b[49m▀ \x1b[m"; lines[1] != want1 {
		t.Errorf("line 1 = %q\nwant     %q", lines[1], want1)
	}
	// A small image grows at most maxGrow times and is centered.
	lines = Draw(&Image{Pix: src}, 40, 40, color.RGBA{})
	if len(lines) != 6 || !strings.HasPrefix(lines[0], strings.Repeat(" ", 16)+"\x1b[") {
		t.Errorf("%d lines, first %q", len(lines), lines[0])
	}
	// Half-transparent pixels blend onto the background.
	if c, ok := pixel(imageOf(color.RGBA{100, 0, 0, 200}), 0, 0, color.RGBA{255, 255, 255, 255}); !ok || c.R != 155 || c.G != 55 {
		t.Errorf("blend = %v %v", c, ok)
	}
}

func TestShrink(t *testing.T) {
	src := image.NewNRGBA(image.Rect(10, 10, 15, 13)) // odd sizes, offset origin
	for y := 10; y < 13; y++ {
		for x := 10; x < 15; x++ {
			c := color.NRGBA{200, 0, 0, 255}
			if x >= 13 {
				c = color.NRGBA{0, 0, 0, 0}
			}
			src.SetNRGBA(x, y, c)
		}
	}
	dst := shrink(src, 2, 1)
	// Left: three red columns. Right: two transparent ones.
	if got := dst.RGBAAt(0, 0); got != (color.RGBA{200, 0, 0, 255}) {
		t.Errorf("left = %v", got)
	}
	if got := dst.RGBAAt(1, 0); got != (color.RGBA{}) {
		t.Errorf("right = %v", got)
	}
	if got := shrink(src, 5, 3); got.RGBAAt(0, 0) != (color.RGBA{200, 0, 0, 255}) || got.Bounds() != image.Rect(0, 0, 5, 3) {
		t.Errorf("same size = %v", got.Bounds())
	}
}

func imageOf(c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.SetRGBA(0, 0, c)
	return img
}

func TestLoader(t *testing.T) {
	var hits sync.Map
	var gate atomic.Bool
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("User-agent: *\nDisallow: /private/\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		n, _ := hits.LoadOrStore(r.URL.Path, new(atomic.Int32))
		n.(*atomic.Int32).Add(1)
		switch r.URL.Path {
		case "/slow.png":
			if gate.Load() {
				<-release
			}
			w.Write(pngOf(4, 4, color.White))
		case "/cat.png":
			// No Content-Type, so a text path would try to convert it.
			w.Header()["Content-Type"] = nil
			w.Write(pngOf(4, 4, color.Black))
		case "/logo.svg":
			w.Write([]byte(`<svg/>`))
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	l := New(fetch.New(fetch.Options{Hosts: []string{u.Host}, Rate: 1000, Burst: 100}))
	ctx := context.Background()
	count := func(p string) int32 {
		n, ok := hits.Load(p)
		if !ok {
			return 0
		}
		return n.(*atomic.Int32).Load()
	}

	for range 3 {
		img, err := l.Load(ctx, srv.URL+"/cat.png")
		if err != nil || img.Pix.RGBAAt(0, 0) != (color.RGBA{0, 0, 0, 255}) {
			t.Fatalf("cat = %v, %v", img, err)
		}
	}
	if n := count("/cat.png"); n != 1 {
		t.Errorf("cat fetched %d times", n)
	}

	// Failures are remembered for a while, then tried again.
	now := time.Now()
	l.now = func() time.Time { return now }
	for range 2 {
		if _, err := l.Load(ctx, srv.URL+"/logo.svg"); !errors.Is(err, ErrUnsupported) {
			t.Errorf("svg: %v", err)
		}
	}
	if _, err := l.Load(ctx, srv.URL+"/missing.png"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing: %v", err)
	}
	if n := count("/logo.svg"); n != 1 {
		t.Errorf("svg fetched %d times", n)
	}
	now = now.Add(failFor + time.Second)
	l.Load(ctx, srv.URL+"/logo.svg")
	if n := count("/logo.svg"); n != 2 {
		t.Errorf("svg fetched %d times after it expired", n)
	}
	// Images that loaded are fetched again after keepFor.
	l.Load(ctx, srv.URL+"/cat.png")
	if n := count("/cat.png"); n != 1 {
		t.Errorf("cat fetched %d times", n)
	}
	now = now.Add(keepFor)
	l.Load(ctx, srv.URL+"/cat.png")
	if n := count("/cat.png"); n != 2 {
		t.Errorf("cat fetched %d times after it expired", n)
	}

	// robots.txt and the allowlist apply.
	if _, err := l.Load(ctx, srv.URL+"/private/x.png"); !errors.Is(err, fetch.ErrDisallowed) {
		t.Errorf("private: %v", err)
	}
	if _, err := l.Load(ctx, "https://elsewhere.example/x.png"); !errors.Is(err, fetch.ErrNotAllowed) {
		t.Errorf("elsewhere: %v", err)
	}
	if _, err := l.Load(ctx, "data:image/png;base64,AAAA"); err == nil {
		t.Error("data URL loaded")
	}

	// Sessions waiting on the same image share one fetch, and one that
	// gives up doesn't cancel it for the others.
	gate.Store(true)
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := l.Load(short, srv.URL+"/slow.png"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("short wait: %v", err)
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if _, err := l.Load(ctx, srv.URL+"/slow.png"); err != nil {
				t.Errorf("slow: %v", err)
			}
		})
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := count("/slow.png"); n != 1 {
		t.Errorf("slow fetched %d times", n)
	}
}

func TestCacheBounded(t *testing.T) {
	l := New(nil)
	l.mu.Lock()
	for i := range 200 {
		l.add(&cached{url: string(rune('a' + i)), size: cacheBytes / 50})
	}
	l.mu.Unlock()
	if l.size > cacheBytes || l.lru.Len() != 50 || len(l.byURL) != 50 {
		t.Errorf("size %d, %d entries", l.size, l.lru.Len())
	}
}
