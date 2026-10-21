// Package images fetches a site's pictures and draws them with half-block
// characters, two pixels to a cell.
package images

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/url"
	"sync"
	"time"

	"golang.org/x/image/webp"

	"github.com/burraksumer/sshr/internal/fetch"
)

// Limits on what a Loader fetches and keeps.
const (
	// maxBytes caps an image file.
	maxBytes = 8 << 20
	// maxPixels caps an image's decoded size, about 48 MB at four bytes a
	// pixel. Images with 16 bits a channel count double.
	maxPixels = 12_000_000
	// maxSide is the largest width or height kept after decoding. Half
	// blocks rarely show more than 300 by 160 pixels, but kitty and Ghostty
	// show real pixels.
	maxSide = 1024
	// cacheBytes bounds the decoded images kept for all sessions.
	cacheBytes = 32 << 20
	// keepFor is how long an image is cached before it is fetched again,
	// and failFor how long a failure is.
	keepFor = time.Hour
	failFor = 5 * time.Minute
	// loadTimeout bounds fetching and decoding one image.
	loadTimeout = 30 * time.Second
	// fetches caps images fetched at once, and decodes those decoded at
	// once, which bounds memory, since each can take maxPixels.
	fetches = 4
	decodes = 1
)

// ErrUnsupported means the file isn't a PNG, JPEG, GIF or WebP image.
var ErrUnsupported = errors.New("not a PNG, JPEG, GIF or WebP image")

// Accept is the Accept header for image requests.
const Accept = "image/png,image/jpeg,image/gif,image/webp;q=0.9"

// Image is a decoded picture, scaled down to at most maxSide on each side.
// It is shared by sessions and never changed.
type Image struct {
	// Pix is premultiplied, as image.RGBA is.
	Pix *image.RGBA
	// Width and Height are the original size. Format is "png", "jpeg",
	// "gif" or "webp".
	Width, Height int
	Format        string
}

// Loader fetches images through a fetch.Client, so they get the same
// allowlist, robots.txt rules and rate limit as the site's pages. Decoded
// images are cached and shared by every session.
type Loader struct {
	client  *fetch.Client
	fetches chan struct{}
	decodes chan struct{}

	mu       sync.Mutex
	lru      *list.List // of *cached, most recent first
	byURL    map[string]*list.Element
	size     int
	inflight map[string]*call
	now      func() time.Time
}

type cached struct {
	url     string
	img     *Image
	err     error
	expires time.Time
	size    int
}

type call struct {
	done chan struct{}
	img  *Image
	err  error
}

// New returns a Loader that fetches with client.
func New(client *fetch.Client) *Loader {
	return &Loader{
		client:   client,
		fetches:  make(chan struct{}, fetches),
		decodes:  make(chan struct{}, decodes),
		lru:      list.New(),
		byURL:    map[string]*list.Element{},
		inflight: map[string]*call{},
		now:      time.Now,
	}
}

// Load returns the image at rawURL. Sessions asking for the same image share
// one fetch, which finishes and is cached even if the session that started
// it stops waiting.
func (l *Loader) Load(ctx context.Context, rawURL string) (*Image, error) {
	l.mu.Lock()
	if el, ok := l.byURL[rawURL]; ok {
		c := el.Value.(*cached)
		if l.now().Before(c.expires) {
			l.lru.MoveToFront(el)
			l.mu.Unlock()
			return c.img, c.err
		}
		l.remove(el)
	}
	cl, ok := l.inflight[rawURL]
	if !ok {
		cl = &call{done: make(chan struct{})}
		l.inflight[rawURL] = cl
		go l.run(rawURL, cl)
	}
	l.mu.Unlock()

	select {
	case <-cl.done:
		return cl.img, cl.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *Loader) run(rawURL string, cl *call) {
	ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
	defer cancel()
	cl.img, cl.err = l.fetch(ctx, rawURL)

	l.mu.Lock()
	delete(l.inflight, rawURL)
	// A failure is remembered for a while, so a broken image isn't
	// fetched again on every key press.
	c := &cached{url: rawURL, img: cl.img, err: cl.err, size: 256, expires: l.now().Add(keepFor)}
	if cl.err != nil {
		c.expires = l.now().Add(failFor)
	} else {
		c.size += len(cl.img.Pix.Pix)
	}
	l.add(c)
	l.mu.Unlock()
	close(cl.done)
}

// add caches c and drops the least recently used images past cacheBytes.
// The caller holds mu.
func (l *Loader) add(c *cached) {
	l.byURL[c.url] = l.lru.PushFront(c)
	l.size += c.size
	for l.size > cacheBytes && l.lru.Len() > 1 {
		l.remove(l.lru.Back())
	}
}

func (l *Loader) remove(el *list.Element) {
	c := el.Value.(*cached)
	l.lru.Remove(el)
	delete(l.byURL, c.url)
	l.size -= c.size
}

func (l *Loader) fetch(ctx context.Context, rawURL string) (img *Image, err error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("can't fetch %q", rawURL)
	}
	if err := acquire(ctx, l.fetches); err != nil {
		return nil, err
	}
	res, err := l.client.GetBinary(ctx, u, Accept, maxBytes)
	<-l.fetches
	if err != nil {
		return nil, err
	}
	if res.Status != http.StatusOK {
		return nil, fmt.Errorf("the server answered %d", res.Status)
	}
	if err := acquire(ctx, l.decodes); err != nil {
		return nil, err
	}
	defer func() { <-l.decodes }()
	// A decoder that panics on a malformed file fails that image only.
	defer func() {
		if r := recover(); r != nil {
			img, err = nil, fmt.Errorf("decoding failed: %v", r)
		}
	}()
	return decode(res.Body)
}

func acquire(ctx context.Context, slots chan struct{}) error {
	select {
	case slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// decoders are the formats sshr reads. SVG isn't one, since drawing it means
// running a renderer on whatever the file asks for.
var decoders = map[string]struct {
	config func([]byte) (image.Config, error)
	decode func([]byte) (image.Image, error)
}{
	"png":  {func(b []byte) (image.Config, error) { return png.DecodeConfig(bytes.NewReader(b)) }, func(b []byte) (image.Image, error) { return png.Decode(bytes.NewReader(b)) }},
	"jpeg": {func(b []byte) (image.Config, error) { return jpeg.DecodeConfig(bytes.NewReader(b)) }, func(b []byte) (image.Image, error) { return jpeg.Decode(bytes.NewReader(b)) }},
	"gif":  {func(b []byte) (image.Config, error) { return gif.DecodeConfig(bytes.NewReader(b)) }, func(b []byte) (image.Image, error) { return gif.Decode(bytes.NewReader(b)) }},
	"webp": {func(b []byte) (image.Config, error) { return webp.DecodeConfig(bytes.NewReader(b)) }, func(b []byte) (image.Image, error) { return webp.Decode(bytes.NewReader(b)) }},
}

// sniff names an image format from its first bytes. The Content-Type is
// ignored, since servers often get it wrong.
func sniff(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "png"
	case bytes.HasPrefix(b, []byte("\xff\xd8\xff")):
		return "jpeg"
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return "gif"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "webp"
	}
	return ""
}

// decode reads an image, checking its size before decoding the pixels, and
// scales it to fit maxSide.
func decode(body []byte) (*Image, error) {
	format := sniff(body)
	d, ok := decoders[format]
	if !ok {
		return nil, ErrUnsupported
	}
	cfg, err := d.config(body)
	if err != nil {
		return nil, err
	}
	pixels := int64(cfg.Width) * int64(cfg.Height)
	switch cfg.ColorModel {
	case color.RGBA64Model, color.NRGBA64Model, color.Gray16Model:
		pixels *= 2
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || pixels > maxPixels {
		return nil, fmt.Errorf("the image is too large, %d by %d", cfg.Width, cfg.Height)
	}
	src, err := d.decode(body)
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	w, h := fit(b.Dx(), b.Dy(), maxSide, maxSide, 1)
	return &Image{Pix: shrink(src, w, h), Width: cfg.Width, Height: cfg.Height, Format: format}, nil
}

// shrink scales src down to w by h, averaging the source pixels that fall
// in each target pixel. It converts one source row at a time, so it needs
// little memory beyond src itself, unlike a two-pass resampler.
func shrink(src image.Image, w, h int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	if w == sw && h == sh {
		draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
		return dst
	}
	row := image.NewRGBA(image.Rect(0, 0, sw, 1))
	sums := make([]uint64, 4*w)
	cols := make([]uint64, w) // source columns in each target column
	for x := range sw {
		cols[x*w/sw]++
	}
	y := 0
	for ty := range h {
		end := (ty + 1) * sh / h
		rows := uint64(end - y)
		for ; y < end; y++ {
			draw.Draw(row, row.Bounds(), src, image.Pt(b.Min.X, b.Min.Y+y), draw.Src)
			for x := range sw {
				i, p := 4*(x*w/sw), row.Pix[4*x:4*x+4]
				sums[i] += uint64(p[0])
				sums[i+1] += uint64(p[1])
				sums[i+2] += uint64(p[2])
				sums[i+3] += uint64(p[3])
			}
		}
		out := dst.Pix[ty*dst.Stride:]
		for tx := range w {
			n := cols[tx] * rows
			for c := range 4 {
				out[4*tx+c] = uint8(sums[4*tx+c] / n)
				sums[4*tx+c] = 0
			}
		}
	}
	return dst
}

// fit scales w by h to fit within maxW by maxH, keeping its shape and
// growing it by at most grow.
func fit(w, h, maxW, maxH int, grow float64) (int, int) {
	s := min(float64(maxW)/float64(w), float64(maxH)/float64(h), grow)
	return max(1, int(float64(w)*s+0.5)), max(1, int(float64(h)*s+0.5))
}
