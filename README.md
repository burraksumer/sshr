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

1. `robots.txt`, to check what it may fetch and to find sitemaps
2. feed links in the homepage `<head>` (JSON Feed, Atom or RSS), or `/feed.json`, `/feed.xml`, `/atom.xml`, `/rss.xml`, `/index.xml` and `/feed` when there are none
3. the sitemap, for pages the feed doesn't list
4. each page that the feed only summarizes. sshr asks for `text/markdown` first, and when the site sends HTML it extracts the article with Readability

## Run it

With Docker:

```sh
docker build -t sshr .
docker run -d --name sshr -p 22:2222 \
  -e SSHR_URL=https://example.com \
  -v sshr-data:/data \
  sshr
```

From source (Go 1.27):

```sh
go build -o sshr ./cmd/sshr
./sshr serve --url https://example.com --listen :2222
```

The volume holds the host key. Without it, visitors get a "host key changed" warning every time the container is recreated.

To see what sshr finds on a site before you deploy, run `sshr check https://example.com`. To read a site in your own terminal with no server, run `sshr read https://example.com`.

## Pointing a domain at it

SSH sends no hostname, so `ssh example.com` connects to whatever answers on port 22 at that domain's IP. Most sites put their main domain on a CDN or a host like Vercel or Netlify, which don't accept SSH. Use a subdomain with a plain DNS record pointing at the machine that runs sshr:

```
ssh.example.com.  A  203.0.113.10
```

If you use Cloudflare, that record has to be DNS-only (grey cloud). Cloudflare's proxy only forwards HTTP. A DNS-only record does publish the server's real IP.

If the machine already runs OpenSSH on port 22 for your own logins, move one of them. Either OpenSSH goes to another port and sshr takes 22, or sshr stays on 2222 and visitors run `ssh -p 2222 ssh.example.com`.

## Configuration

Everything except the URL has a default. See [`sshr.example.toml`](sshr.example.toml) for every option. sshr reads `sshr.toml` from the working directory, or the file named by `--config` or `SSHR_CONFIG`. `SSHR_URL`, `SSHR_LISTEN` and `SSHR_HOST_KEY` override the file. Set `SSHR_DEBUG=1` to log every fetch.

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

Links are numbered in the text, and the full URLs are listed at the end of each entry. A link to another page on the same site opens in the reader. Terminals that support OSC 8 make the URLs clickable.

## Security

Visitors are anonymous strangers, so sshr is built to give them nothing but the reader:

- It never runs a shell or any other program. Commands, SFTP and other subsystems, port forwarding and agent forwarding are all refused.
- Any public key is accepted after the SSH layer verifies its signature. Visitors without keys get keyboard-interactive auth with no prompts.
- sshr strips control and escape bytes from everything it fetches, so a post can't send escape codes to a visitor's terminal. Only sshr's renderer writes escape sequences.
- It only fetches from the site's own host (plus `www.`) and `allowed_hosts`, redirects included. That keeps the page fetcher from reaching internal addresses.
- Concurrent connections and sessions are capped per IP and in total. New sessions per IP are rate limited. Sessions end after an idle timeout and a maximum duration.
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
