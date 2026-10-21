# sshr

sshr serves a website's posts and pages as a terminal reader over SSH. Point it at a URL and visitors run `ssh ssh.example.com` to get a list of everything on the site. They pick an entry with the arrow keys or j/k and read it as formatted text.

```
 Burak Sümer
 Personal blog and notes.

  Why did I switch to Astro?                              2023-02-07
  Letter 2: Awakening                                     2022-03-03
  My search for minimal web browsers                      2022-03-02
  Letter 1: Losing Game                                   2022-03-01

 ↑↓/jk move · enter read · / search · ? help · q quit
```

The site doesn't need any changes. sshr reads what it already publishes:

1. `robots.txt`, to check what it may fetch, how fast it may go and where the sitemaps are
2. feed links in the homepage `<head>`, preferring JSON Feed, then Atom, then RSS, and then the common paths `/feed.json`, `/feed.xml`, `/atom.xml`, `/rss.xml`, `/index.xml` and `/feed`
3. the sitemap, for pages the feed doesn't list
4. each page that the feed only summarizes. sshr asks for `text/markdown` first, and when the site sends HTML it extracts the article with Readability

A site URL with a path, like `https://example.com/blog`, limits sshr to that part of the site. A site that wants more say in how it looks can publish a [manifest](#site-manifest).

## Run it

With Docker:

```sh
docker build -t sshr .
docker run -d --name sshr -p 22:2222 \
  -e SSHR_URL=https://example.com \
  -v sshr-data:/data \
  sshr
```

The volume holds the host key. Without it, visitors get a "host key changed" warning every time the container is recreated.

From source, with Go 1.27:

```sh
go build -o sshr ./cmd/sshr
./sshr serve --url https://example.com --listen :2222
```

To see what sshr finds on a site before you deploy, run `sshr check https://example.com`. To read a site in your own terminal with no server, run `sshr read https://example.com`.

## Pointing a domain at it

SSH sends no hostname, so `ssh example.com` connects to whatever answers on port 22 at that domain's IP. Most sites put their main domain on a CDN or a host like Vercel or Netlify, which don't accept SSH. Use a subdomain with a plain DNS record pointing at the machine that runs sshr:

```
ssh.example.com.  A  203.0.113.10
```

On Cloudflare, that record has to be DNS-only, shown as a grey cloud, because Cloudflare's proxy only forwards HTTP. A DNS-only record does publish the server's real IP.

If the machine already runs OpenSSH on port 22 for your own logins, move one of them. Either OpenSSH goes to another port and sshr takes 22, or sshr stays on 2222 and visitors run `ssh -p 2222 ssh.example.com`.

## Configuration

Everything except the URL has a default. See [`sshr.example.toml`](sshr.example.toml) for every option.

sshr reads `sshr.toml` from the working directory if it exists. A file named with `--config` or `SSHR_CONFIG` must exist. In Docker the working directory is `/data`, so mount the file at `/data/sshr.toml`. `SSHR_URL`, `SSHR_LISTEN`, `SSHR_HOST_KEY` and `SSHR_LOG_FORMAT` override the file, and `--url` and `--listen` override both. Set `SSHR_DEBUG=1` to also log fetches that fail.

## Site manifest

A site can publish `/.well-known/sshr.json` to set its menu, logo and color, and to pick which pages sshr shows. Every field is optional. [`schema/sshr.schema.json`](schema/sshr.schema.json) describes the format, and editors that read JSON Schema will check the file if it starts with `"$schema": "https://raw.githubusercontent.com/burraksumer/sshr/main/schema/sshr.schema.json"`.

```json
{
  "title": "Example",
  "description": "Notes and posts.",
  "accent": "#e8590c",
  "logo": " _____\n| ____|_  __\n|  _| \\ \\/ /\n| |___ >  <\n|_____/_/\\_\\",
  "feed": "/feed.xml",
  "pages": ["/about", "/now"],
  "exclude": ["/tags/", "/drafts/"],
  "sitemap": false,
  "nav": [
    { "label": "Posts", "show": "posts" },
    { "label": "Pages", "show": "pages" },
    { "label": "About", "page": "/about" },
    { "label": "GitHub", "url": "https://github.com/example" }
  ]
}
```

- `title` and `description` replace what sshr reads from the homepage and feed.
- `logo` is ASCII art above the title. sshr shows it only when the terminal has room for it above the list, keeps up to 16 lines of 100 characters, and strips control characters.
- `accent` is a `#rrggbb` color for the title, keys and selection.
- `feed` is tried before the feeds the homepage links.
- `pages` are included even when the sitemap doesn't list them, and come first among pages in this order.
- `exclude` drops posts and pages whose path starts with one of these prefixes.
- `sitemap: false` stops sshr from adding pages from the sitemap, so only the feed, `pages` and menu pages are listed.
- `nav` is a menu of up to nine items, picked with the number keys. An item with `show` filters the list to `all`, `posts` or `pages`, and the list starts on the first one. An item with `page` opens that page, which sshr includes without it being listed in `pages`. An item with `url` shows the link in the footer.

sshr fetches the manifest on every refresh, with the same robots.txt rules and allowed hosts as the rest of the site. When the fetch fails for a moment it keeps the last copy. A manifest over 64 KiB or one that isn't valid JSON counts as missing.

The `[site]` section of `sshr.toml` takes the same fields and wins over the site's manifest field by field. That's also how to set a menu for a site you don't control.

## Keys

| List | |
|---|---|
| `↑` `↓` `j` `k` | move |
| `enter` `l` | read |
| `/` | search titles, summaries and tags |
| `g` `G` | first, last |
| `1` to `9` | menu items, when the site has a menu |

| Reader | |
|---|---|
| `↑` `↓` `j` `k`, `space` `b` | scroll |
| `3` `enter` | follow link [3] |
| `h` `esc` `q` | back |

Links are numbered in the text, and the full URLs are listed at the end of each entry. A link to another page on the same site opens in the reader. In terminals that support OSC 8, http, https and mailto links are clickable. Other schemes are shown as text only.

## Security

Visitors are anonymous strangers, so sshr is built to give them nothing but the reader:

- It never runs a shell or any other program. Commands, SFTP and other subsystems, port forwarding and agent forwarding are all refused.
- Any public key is accepted after the SSH layer verifies its signature. Visitors without keys get keyboard-interactive auth with no prompts.
- sshr strips control and escape bytes from everything it fetches. It also neutralizes character references like `&#27;` that the renderer would decode into them, and it filters the rendered output so only its own styling and links get through. A post can't send escape codes to a visitor's terminal.
- It only fetches from the site's own host and its `www.` variant, plus `allowed_hosts`. That means ports 80 and 443, or the port in the site URL, and redirects get the same check. Content can't point the fetcher at any host or port outside that list.
- It follows each host's robots.txt, on redirects too. When robots.txt can't be fetched or answers with a server error, sshr fetches nothing from that host.
- Connections and sessions are capped per client and in total. IPv6 clients count per /64. New sessions per client are rate limited, and a connection that doesn't open a session within 30 seconds is closed. Sessions end after an idle timeout and a maximum duration.
- Visitors only ever read from the in-memory snapshot. Fetching happens in the background on a timer, so a flood of visitors never turns into requests to your site.
- Logs record IPs and session length, never key fingerprints.

## Resource use

These were measured on Linux with a small blog:

| | |
|---|---|
| binary | 27 MB |
| Docker image | 22 MB |
| memory, idle | 29 MB |
| memory, 50 open sessions | 95 MB |

## License

MIT
