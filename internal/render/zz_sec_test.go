package render

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/burraksumer/sshr/internal/sanitize"
	"github.com/charmbracelet/x/ansi"
)

func secCheck(t *testing.T, in, out string) {
	t.Helper()
	plain := strings.Join(strings.Fields(ansi.Strip(out)), "")
	for i := 0; i < len(out); {
		c := out[i]
		if c == 0x1b {
			if i+1 >= len(out) {
				t.Fatalf("trailing ESC for %q", in)
			}
			switch out[i+1] {
			case '[':
				j := i + 2
				for j < len(out) && (out[j] >= '0' && out[j] <= '9' || out[j] == ';' || out[j] == ':') {
					j++
				}
				if j >= len(out) || out[j] != 'm' {
					t.Fatalf("non-SGR CSI %q for %q", out[i:min(j+1, len(out))], in)
				}
				i = j + 1
				continue
			case ']':
				k := strings.IndexAny(out[i+2:], "\x07\x1b")
				if k < 0 {
					t.Fatalf("unterminated OSC for %q", in)
				}
				body := out[i+2 : i+2+k]
				end := i + 2 + k
				if out[end] == 0x07 {
					end++
				} else if end+1 < len(out) && out[end+1] == '\\' {
					end += 2
				} else {
					t.Fatalf("bad OSC terminator for %q", in)
				}
				rest, ok := strings.CutPrefix(body, "8;")
				if !ok {
					t.Fatalf("non-OSC8 %q for %q", body, in)
				}
				_, target, ok := strings.Cut(rest, ";")
				if !ok {
					t.Fatalf("bad OSC8 %q", body)
				}
				for x := 0; x < len(target); x++ {
					if target[x] < 0x21 || target[x] > 0x7e {
						t.Fatalf("non-printable target %q for %q", target, in)
					}
				}
				if target != "" {
					if !Clickable(target) {
						t.Fatalf("non-clickable target %q for %q", target, in)
					}
					vis := strings.TrimPrefix(target, "mailto:")
					if !strings.Contains(plain, vis) {
						t.Fatalf("target %q not visible in %q for input %q", target, plain, in)
					}
				}
				i = end
				continue
			default:
				t.Fatalf("other ESC %q for %q", out[i:i+2], in)
			}
		}
		r, size := utf8.DecodeRuneInString(out[i:])
		if r == utf8.RuneError && size == 1 {
			t.Fatalf("invalid utf8 for %q", in)
		}
		if sanitize.Dropped(r) {
			t.Fatalf("control %U in output for %q", r, in)
		}
		i += size
	}
}

var secSeeds = []string{
	"&#27;[31m red &#x1b;]8;;http://evil/&#7;x",
	"`&#27;[2J` and `&#x9b;2J`",
	"~~&#2~~7;~~ ~~a&#27;b~~",
	"<div>&#27;]0;title&#7;</div>\n\n<span>&#x9d;0;x&#x9c;</span>",
	"<plaintext>&#27;[2J",
	"![https://bank.example](https://evil.example/x.png)",
	"![alt](&#x6a;avascript:alert(1))",
	"[a](http://x/&#x1b;) [b](<http://y/\u0085>)",
	"<http://evil.example/&#27;[2J> www.evil.example/&#x9b;",
	"<foo@bar.example> mailto:foo@bar.example",
	"| a | b |\n|---|---|\n| [x](http://t/) | ![i](http://img/) |",
	"# &#27;[1mHead\n\nTerm\n: &#x1b;def",
	"[x][r]\n\n[r]: http://ref/&#27;\n",
	"&#128;&#129;&#141;&#143;&#144;&#157;&#x80;&#x9F;&#0;&#xD800;&#1114112;",
	"&NewLine;&Tab;&#10;&#13;&#9;",
	"![a&#27;b](http://i/ \"t&#27;\")",
	"<img alt=\"&#27;\" src=\"http://x\">&#27;</img>",
	"\x1b]8;;http://evil/\x07click\x1b]8;;\x07 \x9b2J \xc2\x9b2J",
	"<a href=\"http://evil\">\x1b[2Jtxt</a>",
	"* [l](http://l/)\n  > <b>&#27;</b>\n",
	"<span title=\"&#2\n7;\">x</span>",
	"> <div>\n> &#2\n> 7;\n> </div>",
	"&#x0000001b;&#00000027&#27&#x1b",
}

func FuzzSecRender(f *testing.F) {
	for _, s := range secSeeds {
		f.Add(s, true)
		f.Add(s, false)
	}
	f.Fuzz(func(t *testing.T, s string, clean bool) {
		in := s
		if clean {
			in = sanitize.Text(s)
		}
		for _, dark := range []bool{true, false} {
			doc, err := Markdown(in, Options{Width: 60, Dark: dark, BaseURL: "https://site.example/blog/post"})
			if err != nil {
				return
			}
			if clean {
				secCheck(t, in, doc.Content)
			} else {
				// raw input: only check no non-SGR/OSC8 escapes and no controls
				secCheckLoose(t, in, doc.Content)
			}
			for _, l := range doc.Links {
				for x := 0; x < len(l.URL); x++ {
					if l.URL[x] < 0x21 || l.URL[x] > 0x7e {
						t.Fatalf("link URL %q not printable for %q", l.URL, in)
					}
				}
			}
		}
	})
}

func secCheckLoose(t *testing.T, in, out string) {
	t.Helper()
	// Replace allowed-target visibility requirement: just structure.
	for i := 0; i < len(out); {
		if out[i] == 0x1b {
			switch out[i+1] {
			case '[':
				j := i + 2
				for j < len(out) && (out[j] >= '0' && out[j] <= '9' || out[j] == ';' || out[j] == ':') {
					j++
				}
				if out[j] != 'm' {
					t.Fatalf("non-SGR for %q", in)
				}
				i = j + 1
				continue
			case ']':
				k := strings.IndexAny(out[i+2:], "\x07\x1b")
				body := out[i+2 : i+2+k]
				if !strings.HasPrefix(body, "8;") {
					t.Fatalf("non-OSC8 for %q", in)
				}
				for x := 0; x < len(body); x++ {
					if body[x] < 0x20 || body[x] > 0x7e {
						t.Fatalf("np body")
					}
				}
				i = i + 2 + k + 1
				if out[i-1] == 0x1b {
					i++
				}
				continue
			default:
				t.Fatalf("other ESC for %q", in)
			}
		}
		r, size := utf8.DecodeRuneInString(out[i:])
		if (r == utf8.RuneError && size == 1) || sanitize.Dropped(r) {
			t.Fatalf("control %U for %q", r, in)
		}
		i += size
	}
}

func TestSecPrint(t *testing.T) {
	for _, s := range []string{secSeeds[5], secSeeds[8], secSeeds[9], secSeeds[17]} {
		d, _ := Markdown(s, Options{Width: 60, Dark: true, BaseURL: "https://site.example/blog/post"})
		t.Logf("%q", d.Content)
	}
}
