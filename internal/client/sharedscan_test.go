package client

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/transport/sshtest"
)

// countScans counts buildManifest's scans for the test; gate, when set,
// holds each scan until it is closed, after a send on started.
func countScans(t *testing.T, gate, started chan struct{}) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	old := buildManifest
	buildManifest = func(ctx context.Context, o manifest.Options) (*manifest.Manifest, error) {
		n.Add(1)
		if gate != nil {
			started <- struct{}{}
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return old(ctx, o)
	}
	t.Cleanup(func() { buildManifest = old })
	return &n
}

// scanRoot is a project directory holding a.txt.
func scanRoot(t *testing.T) *Workspace {
	t.Helper()
	root := t.TempDir()
	for name, body := range map[string]string{".spotexec.toml": "version = 1\n", "a.txt": "A"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

// Every part of a fan-out gets the one scan, as a copy of its own: a part
// that stops waiting (fail_fast, Ctrl-C on its own) doesn't end it for the
// others.
func TestSharedScanIsTakenOnceForEveryPart(t *testing.T) {
	gate, started := make(chan struct{}), make(chan struct{}, 1)
	scans := countScans(t, gate, started)
	ws := scanRoot(t)
	scan := NewSharedScan(context.Background(), ws, &config.Project{})
	defer scan.Close()

	// The first part starts the scan, then stops waiting for it.
	first, stop := context.WithCancel(context.Background())
	firstErr := make(chan error, 1)
	go func() { _, err := scan.get(first); firstErr <- err }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the first part never started the scan")
	}
	var wg sync.WaitGroup
	got := make([]*manifest.Manifest, 3)
	errs := make([]error, 3)
	for i := range got {
		wg.Add(1)
		go func() { defer wg.Done(); got[i], errs[i] = scan.get(context.Background()) }()
	}
	stop()
	select {
	case err := <-firstErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the part that stopped waiting: %v", err)
		}
	case <-time.After(10 * time.Second):
		close(gate)
		t.Fatal("the part that stopped waiting is still waiting")
	}
	close(gate)
	wg.Wait()
	if n := scans.Load(); n != 1 {
		t.Fatalf("%d scans for 4 parts", n)
	}
	for i, m := range got {
		if errs[i] != nil || m == nil || m.ByPath["a.txt"].ID == "" {
			t.Fatalf("part %d: %v, %v", i, m, errs[i])
		}
	}
	// One part's corrections are its own.
	was := got[1].ByPath["a.txt"]
	got[0].Update(protocol.Entry{Path: "a.txt", ID: "changed", Mode: was.Mode, Size: 7})
	if got[1].ByPath["a.txt"] != was || got[2].ByPath["a.txt"] != was {
		t.Fatal("an update to one part's copy reached another's")
	}
	if late, err := scan.get(context.Background()); err != nil || late.ByPath["a.txt"] != was || scans.Load() != 1 {
		t.Fatalf("a later part: %v, %v after %d scans", late, err, scans.Load())
	}
}

// A failed scan fails every part that syncs, each with the scan's error.
func TestSharedScanErrorReachesEveryPart(t *testing.T) {
	scans := countScans(t, nil, nil)
	ws := scanRoot(t)
	scan := NewSharedScan(context.Background(), ws, &config.Project{Sync: config.Sync{Source: "git"}})
	defer scan.Close()
	for i := range 3 {
		if _, err := scan.get(context.Background()); err == nil || !strings.Contains(err.Error(), "not a git worktree") {
			t.Fatalf("part %d: %v", i, err)
		}
	}
	if n := scans.Load(); n != 1 {
		t.Fatalf("%d scans", n)
	}
}

// A file edited after the shared scan uploads with its new bytes: the part
// that sent them takes them as its write-back baseline, while its siblings
// (whose slots never got them) keep the scanned ones.
func TestSharedScanUploadCorrectionStaysWithItsPart(t *testing.T) {
	ws := scanRoot(t)
	scan := NewSharedScan(context.Background(), ws, &config.Project{})
	defer scan.Close()
	sent, err := scan.get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := scan.get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	scanned := sibling.ByPath["a.txt"]
	if err := os.WriteFile(filepath.Join(ws.Root, "a.txt"), []byte("BB"), 0o644); err != nil {
		t.Fatal(err)
	}

	sshtest.Install(t).Set("rpc-sync", "a.txt")
	r := &config.Runner{Name: "box", Type: config.TypeSSH, Hosts: []string{"box"}, MaxSlots: 2}
	tgt := provider.Target{Host: "box", User: "dev"}
	_, s, err := Connect(context.Background(), r, tgt)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Abort()
	if _, err := syncIn(context.Background(), s, sent, nil, nil, Options{Rep: quietRep(t)}); err != nil {
		t.Fatal(err)
	}
	if e := sent.ByPath["a.txt"]; e.ID != fsid.Bytes([]byte("BB")) || e.Size != 2 || scanned.ID != fsid.Bytes([]byte("A")) {
		t.Fatalf("the uploading part's baseline: %+v (scanned %+v)", e, scanned)
	}
	if sibling.ByPath["a.txt"] != scanned {
		t.Fatalf("the sibling's baseline moved to %+v", sibling.ByPath["a.txt"])
	}
	if sent.Fingerprint != sibling.Fingerprint {
		t.Fatal("the parts advertised different scans")
	}
	if later, err := scan.get(context.Background()); err != nil || later.ByPath["a.txt"] != scanned {
		t.Fatalf("a later part: %v, %v", later, err)
	}
}

// Only a part's first attempt starts from the shared scan: a replay or a
// move scans the worktree afresh. (A Dockerfile missing from the working set
// stops each attempt right after its scan.)
func TestOnlyAFirstAttemptUsesTheSharedScan(t *testing.T) {
	scans := countScans(t, nil, nil)
	ws := scanRoot(t)
	p := &config.Project{Container: config.Container{Dockerfile: ".spotexec/Missing"}}
	cfg := &config.Config{Global: &config.Global{Runners: map[string]*config.Runner{"box": {Name: "box", Type: "ssh", Host: "nobody@127.0.0.1:1"}}}, Project: p}
	t.Setenv("SPOTEXEC_POOL", "")
	scan := NewSharedScan(context.Background(), ws, p)
	defer scan.Close()
	rep := &scanRep{}
	o := Options{Sync: "auto", Writeback: "always", Rep: rep, Scan: scan}
	// try runs one attempt and returns how many files its scan held.
	try := func(att attempt) int {
		t.Helper()
		att.runID, att.exclude, att.fresh = "run", map[string]bool{}, new(bool)
		if _, err, _ := runOnce(context.Background(), cfg, ws, o, att); err == nil || !strings.Contains(err.Error(), "not in the synced working set") {
			t.Fatalf("%+v: %v", att, err)
		}
		return rep.files[len(rep.files)-1]
	}
	add := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ws.Root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	scanned := try(attempt{n: 1})
	add("b.txt")
	for range 2 {
		if n := try(attempt{n: 1}); n != scanned {
			t.Fatalf("a first attempt saw %d files, the shared scan %d", n, scanned)
		}
	}
	if n := scans.Load(); n != 1 {
		t.Fatalf("three first attempts: %d scans", n)
	}
	if n := try(attempt{n: 2}); n != scanned+1 {
		t.Fatalf("a replay saw %d files, want %d", n, scanned+1)
	}
	add("c.txt")
	if n := try(attempt{n: 1, moves: 1}); n != scanned+2 {
		t.Fatalf("a move saw %d files, want %d", n, scanned+2)
	}
	if n := scans.Load(); n != 3 {
		t.Fatalf("a replay and a move: %d scans in all, want 3", n)
	}
	// Unsynced, a part never scans.
	o.Sync = "none"
	if _, err, _ := runOnce(context.Background(), cfg, ws, o, attempt{runID: "run", n: 1, exclude: map[string]bool{}, fresh: new(bool)}); err == nil {
		t.Fatal("unsynced: the missing Dockerfile passed")
	}
	if n := scans.Load(); n != 3 {
		t.Fatalf("an unsynced part scanned: %d", n)
	}
}

// scanRep records the file count of each manifest event.
type scanRep struct {
	nopRep
	files []int
}

func (r *scanRep) Event(name string, f map[string]any) {
	if name == "manifest" {
		r.files = append(r.files, f["files"].(int))
	}
}
