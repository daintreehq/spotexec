package tailbuf

import (
	"strings"
	"testing"
)

func TestKeepsEverythingUnderTheLimit(t *testing.T) {
	b := New(10)
	write(t, b, "abc")
	write(t, b, "def")
	if got := string(b.Bytes()); got != "abcdef" {
		t.Fatalf("got %q", got)
	}
}

// write checks the io.Writer contract: a short count would make io.Copy
// stop, and block the process writing.
func write(t *testing.T, b *Buffer, s string) {
	t.Helper()
	if n, err := b.Write([]byte(s)); n != len(s) || err != nil {
		t.Fatalf("Write(%q) = %d, %v", s, n, err)
	}
}

func TestKeepsTheTail(t *testing.T) {
	b := New(5)
	write(t, b, "abc")
	write(t, b, "defg")
	got := string(b.Bytes())
	if !strings.HasSuffix(got, "\ncdefg") || !strings.Contains(got, "2 earlier bytes") {
		t.Fatalf("got %q", got)
	}
	write(t, b, "0123456789")
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
