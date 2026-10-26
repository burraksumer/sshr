package content

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/burraksumer/sshr/internal/fetch"
	"github.com/burraksumer/sshr/internal/sanitize"
)

// ManifestPath is where a site publishes its manifest.
const ManifestPath = "/.well-known/sshr.json"

// Manifest is a site's own description of how sshr should show it. A site
// publishes it at ManifestPath, and an operator can set the same fields in
// the [site] section of sshr.toml, which win field by field. Every field is
// optional.
type Manifest struct {
	Title       string `json:"title" toml:"title"`
	Description string `json:"description" toml:"description"`
	// Logo is ASCII art shown above the title on tall enough terminals.
	Logo string `json:"logo" toml:"logo"`
	// Accent is a "#rrggbb" color for titles, keys and the selection.
	Accent string `json:"accent" toml:"accent"`
	// Feed names the feed to use, ahead of any the homepage links.
	Feed string `json:"feed" toml:"feed"`
	// Pages are extra pages to include, like /about, listed first among
	// pages in this order.
	Pages []string `json:"pages" toml:"pages"`
	// Exclude lists path prefixes, like /tags/, whose pages are left out.
	Exclude []string `json:"exclude" toml:"exclude"`
	// Sitemap set to false stops sshr from adding pages from the sitemap.
	Sitemap *bool `json:"sitemap" toml:"sitemap"`
	// Nav is the menu, up to nine items picked with the number keys.
	Nav []NavItem `json:"nav" toml:"nav"`
}

// NavItem is one menu entry. Set exactly one of Show, Page or URL.
type NavItem struct {
	Label string `json:"label" toml:"label"`
	// Show is "all", "posts" or "pages" and filters the list.
	Show string `json:"show" toml:"show"`
	// Page opens a page on the site, like /about.
	Page string `json:"page" toml:"page"`
	// URL is an http, https or mailto link, shown so the visitor can open it.
	URL string `json:"url" toml:"url"`
}

// NavKind says what a menu item does.
type NavKind int

const (
	NavAll NavKind = iota
	NavPosts
	NavPages
	NavPage
	NavLink
)

// Nav is a checked menu item, ready to show.
type Nav struct {
	Label string
	Kind  NavKind
	// URL is the page to open for NavPage, or the link for NavLink. It is
	// printable ASCII.
	URL string
}

// Limits on what a manifest may hold.
const (
	maxManifestBytes = 64 << 10
	maxNav           = 9
	maxLogoLines     = 16
	maxLogoWidth     = 100
	maxManifestPages = 50
)

var accentColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// siteManifest is a manifest checked against a site's base URL. URLs are
// absolute and encoded, and text is sanitized.
type siteManifest struct {
	title, description string
	logo               []string
	accent             string
	feed               string
	pages              []string
	exclude            []string
	sitemap            bool
	nav                []Nav
}

// merge returns local's fields where they are set, and remote's otherwise.
func merge(local, remote *Manifest) Manifest {
	var m Manifest
	if remote != nil {
		m = *remote
	}
	if local == nil {
		return m
	}
	pick := func(l, r string) string {
		if strings.TrimSpace(l) != "" {
			return l
		}
		return r
	}
	m.Title = pick(local.Title, m.Title)
	m.Description = pick(local.Description, m.Description)
	m.Logo = pick(local.Logo, m.Logo)
	m.Accent = pick(local.Accent, m.Accent)
	m.Feed = pick(local.Feed, m.Feed)
	if local.Pages != nil {
		m.Pages = local.Pages
	}
	if local.Exclude != nil {
		m.Exclude = local.Exclude
	}
	if local.Sitemap != nil {
		m.Sitemap = local.Sitemap
	}
	if local.Nav != nil {
		m.Nav = local.Nav
	}
	return m
}

// check turns a manifest into what a site uses, dropping anything invalid.
// A manifest comes from the site and is untrusted like the rest of its
// content.
func (m Manifest) check(base *url.URL) siteManifest {
	s := siteManifest{
		title:       truncate(sanitize.Line(m.Title), 200),
		description: truncate(sanitize.Line(m.Description), 300),
		logo:        logoLines(m.Logo),
		sitemap:     m.Sitemap == nil || *m.Sitemap,
	}
	if accentColor.MatchString(m.Accent) {
		s.accent = strings.ToLower(m.Accent)
	}
	s.feed = resolveURL(base, m.Feed)
	for _, p := range m.Pages {
		if len(s.pages) == maxManifestPages {
			break
		}
		if u := resolveURL(base, p); u != "" {
			s.pages = append(s.pages, u)
		}
	}
	for _, p := range m.Exclude {
		if p = strings.TrimSpace(p); strings.HasPrefix(p, "/") {
			s.exclude = append(s.exclude, p)
		}
	}
	for _, n := range m.Nav {
		if len(s.nav) == maxNav {
			break
		}
		label := truncate(sanitize.Line(n.Label), 40)
		if label == "" {
			continue
		}
		switch {
		case n.Show != "":
			kind, ok := map[string]NavKind{"all": NavAll, "posts": NavPosts, "pages": NavPages}[strings.ToLower(strings.TrimSpace(n.Show))]
			if ok {
				s.nav = append(s.nav, Nav{Label: label, Kind: kind})
			}
		case n.Page != "":
			if u := resolveURL(base, n.Page); u != "" {
				s.nav = append(s.nav, Nav{Label: label, Kind: NavPage, URL: u})
				// A menu page is included even if Pages doesn't list it.
				if !slices.Contains(s.pages, u) {
					s.pages = append(s.pages, u)
				}
			}
		case n.URL != "":
			u, err := url.Parse(strings.TrimSpace(n.URL))
			if err == nil && (u.Scheme == "http" || u.Scheme == "https" || u.Scheme == "mailto") {
				s.nav = append(s.nav, Nav{Label: label, Kind: NavLink, URL: sanitize.URL(u.String())})
			}
		}
	}
	return s
}

// excludes reports whether the entry URL raw is excluded.
func (s siteManifest) excludes(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && raw != "" && s.excluded(u)
}

// apply sets the site's presentation from the manifest. Its title and
// description win over the homepage's and the feed's.
func (s siteManifest) apply(site *Site) {
	if s.title != "" {
		site.Title = s.title
	}
	if s.description != "" {
		site.Description = s.description
	}
	site.Logo, site.Accent, site.Nav = s.logo, s.accent, s.nav
}

// excluded reports whether u's path starts with an excluded prefix.
func (s siteManifest) excluded(u *url.URL) bool {
	for _, p := range s.exclude {
		if strings.HasPrefix(u.Path, p) {
			return true
		}
	}
	return false
}

// logoLines splits ASCII art into at most maxLogoLines lines without
// control characters, each at most maxLogoWidth characters.
func logoLines(logo string) []string {
	logo = strings.ReplaceAll(logo, "\t", "    ")
	lines := strings.Split(sanitize.Text(logo), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > maxLogoLines {
		lines = lines[:maxLogoLines]
	}
	for i, l := range lines {
		l = strings.TrimRight(l, " ")
		if r := []rune(l); len(r) > maxLogoWidth {
			l = string(r[:maxLogoWidth])
		}
		lines[i] = l
	}
	return lines
}

// resolveURL resolves ref against base and returns it encoded, or "" if it
// isn't an http or https URL.
func resolveURL(base *url.URL, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	u, err := base.Parse(ref)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	u.Fragment, u.RawFragment = "", ""
	return sanitize.URL(u.String())
}

// loadManifest fetches the site's manifest. It returns nil when the site has
// none, and last round's when the fetch fails for a moment.
func (d *Discoverer) loadManifest(ctx context.Context, prev *Site, log *slog.Logger) *Manifest {
	u := d.Base.ResolveReference(&url.URL{Path: ManifestPath})
	var old *Manifest
	if prev != nil {
		old = prev.remoteManifest
	}
	res, err := d.Client.Get(ctx, u, "application/json", old != nil)
	switch {
	case err != nil:
		if passing(err) {
			return old
		}
		return nil
	case res.NotModified:
		return old
	case fetch.Transient(res.Status):
		return old
	case res.Status != 200:
		return nil
	}
	if len(res.Body) > maxManifestBytes {
		log.Warn("manifest too large", "url", u.String(), "bytes", len(res.Body))
		d.Client.Forget(u.String())
		return nil
	}
	var m Manifest
	if err := json.NewDecoder(bytes.NewReader(res.Body)).Decode(&m); err != nil {
		log.Warn("manifest", "url", u.String(), "err", err)
		d.Client.Forget(u.String())
		return nil
	}
	log.Info("manifest", "url", u.String())
	return &m
}
