package ui

import (
	"bytes"
	"testing"
)

func TestRewriterSplitAcrossWrites(t *testing.T) {
	var out bytes.Buffer
	r := &Rewriter{W: &out, Old: []byte("/remote/ws"), New: []byte("/Users/me/repo")}
	in := "at /remote/ws/src/a.ts:1\nand /remote/ws/b.ts and /rem"
	for i := 0; i < len(in); i += 3 {
		end := min(i+3, len(in))
		r.Write([]byte(in[i:end]))
	}
	r.Flush()
	want := "at /Users/me/repo/src/a.ts:1\nand /Users/me/repo/b.ts and /rem"
	if out.String() != want {
		t.Fatalf("got %q want %q", out.String(), want)
	}
}
