package sanitize

import "testing"

func TestText(t *testing.T) {
	cases := map[string]string{
		"plain\ntext\tok":          "plain\ntext\tok",
		"title\x1b]0;pwned\x07":    "title]0;pwned",
		"clear\x1b[2J screen":      "clear[2J screen",
		"c1\u009b31m red":          "c131m red",
		"crlf\r\nline":             "crlf\nline",
		"bidi \u202eevil\u202c":    "bidi evil",
		"del\x7f and nul\x00":      "del and nul",
		"emoji 🙂 and ünïcode stay": "emoji 🙂 and ünïcode stay",
	}
	for in, want := range cases {
		if got := Text(in); got != want {
			t.Errorf("Text(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLine(t *testing.T) {
	if got := Line("  a\n\tb  \x1bc "); got != "a b c" {
		t.Errorf("Line = %q", got)
	}
}
