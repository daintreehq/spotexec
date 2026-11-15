package tailbuf

import (
	"strings"
	"testing"
)

func TestKeepsEverythingUnderTheLimit(t *testing.T) {
	b := New(10)
	b.Write([]byte("abc"))
	b.Write([]byte("def"))
	if got := string(b.Bytes()); got != "abcdef" {
		t.Fatalf("got %q", got)
	}
}

func TestKeepsTheTail(t *testing.T) {
	b := New(5)
	b.Write([]byte("abc"))
	b.Write([]byte("defg"))
	got := string(b.Bytes())
	if !strings.HasSuffix(got, "\ncdefg") || !strings.Contains(got, "2 earlier bytes") {
		t.Fatalf("got %q", got)
	}
	b.Write([]byte("0123456789"))
	got = string(b.Bytes())
	if !strings.HasSuffix(got, "\n56789") || !strings.Contains(got, "12 earlier bytes") {
		t.Fatalf("got %q", got)
	}
}

func TestEmpty(t *testing.T) {
	if got := New(4).Bytes(); len(got) != 0 {
		t.Fatalf("got %q", got)
	}
}
