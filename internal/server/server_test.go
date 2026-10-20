package server

import "testing"

func TestClientKey(t *testing.T) {
	cases := map[string]string{
		"192.0.2.7":            "192.0.2.7",
		"2001:db8:1:2:aaaa::1": "2001:db8:1:2::/64",
		"2001:db8:1:2:bbbb::9": "2001:db8:1:2::/64",
		"2001:db8:1:3::1":      "2001:db8:1:3::/64",
		"::ffff:192.0.2.7":     "::ffff:192.0.2.7",
		"not-an-ip":            "not-an-ip",
	}
	for in, want := range cases {
		if got := clientKey(in); got != want {
			t.Errorf("clientKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSafeTerm(t *testing.T) {
	if got := safeTerm("xterm-256color"); got != "xterm-256color" {
		t.Errorf("safeTerm = %q", got)
	}
	if got := safeTerm("x\u009d52;c;aGk=\u009c\x1b"); got != "x?52?c?aGk???" {
		t.Errorf("safeTerm = %q", got)
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter(2, 3)
	r1, ok1 := l.acquire("a")
	_, ok2 := l.acquire("a")
	_, ok3 := l.acquire("a")
	_, ok4 := l.acquire("b")
	_, ok5 := l.acquire("c")
	if !ok1 || !ok2 || ok3 || !ok4 || ok5 {
		t.Fatalf("acquire results %v %v %v %v %v", ok1, ok2, ok3, ok4, ok5)
	}
	r1()
	r1() // releasing twice must not free two slots
	if _, ok := l.acquire("c"); !ok {
		t.Error("slot not freed")
	}
	if _, ok := l.acquire("d"); ok {
		t.Error("double release freed an extra slot")
	}
}
