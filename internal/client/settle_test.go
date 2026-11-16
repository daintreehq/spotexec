package client

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/transport"
)

// In a non-Git project, spotexec's own state under .spotexec/ is ignored
// here but never settled: write-back refuses it, so it must stay pending for
// the next sync to remove. Ordinary ignored outputs are settled (#156).
func TestSettleCreatedKeepsReservedPending(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("dist/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m, err := manifest.Build(ctx, manifest.Options{Root: dir, Source: "directory"})
	if err != nil {
		t.Fatal(err)
	}
	ws := &Workspace{Root: dir}
	var buf bytes.Buffer
	s := &transport.Session{Conn: protocol.NewConn(nil, &buf)}
	created := []protocol.Entry{
		{Path: ".spotexec/conflicts/run/a.txt"},
		{Path: ".spotexec/writeback.lock"},
		{Path: ".spotexec/Dockerfile.new"},
		{Path: "dist/app.js"},
	}
	ign := settleCreated(ctx, s, ws, m, created)
	if !ign[".spotexec/conflicts/run/a.txt"] || !ign["dist/app.js"] || ign[".spotexec/Dockerfile.new"] {
		t.Fatalf("ignored = %v", ign)
	}
	f, err := protocol.NewConn(&buf, nil).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	env, err := protocol.Decode(f)
	if err != nil || env.Type != protocol.TypeSettle {
		t.Fatalf("frame %+v, err %v", env, err)
	}
	var st protocol.Settle
	if err := env.Unmarshal(&st); err != nil {
		t.Fatal(err)
	}
	sort.Strings(st.Ignored)
	if want := []string{"dist/app.js"}; !reflect.DeepEqual(st.Ignored, want) {
		t.Fatalf("settled %v, want %v", st.Ignored, want)
	}
}
