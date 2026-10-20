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

A site URL with a path, like `https://example.com/blog`, limits sshr to that part of the site.

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

sshr reads `sshr.toml` from the working directory if it exists. A file named with `--config` or `SSHR_CONFIG` must exist. In Docker the working directory is `/data`, so mount the file at `/data/sshr.toml`. `SSHR_URL`, `SSHR_LISTEN`, `SSHR_HOST_KEY` and `SSHR_LOG_FORMAT` override the file, and `--url` and `--listen` override both. Set `SSHR_DEBUG=1` to log every fetch.

## Keys

| List | |
|---|---|
| `↑` `↓` `j` `k` | move |
| `enter` `l` | read |
| `/` | search titles, summaries and tags |
| `g` `G` | first, last |

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
| memory, idle | 30 MB |
| memory, 50 open sessions | 86 MB |

## License

MIT
