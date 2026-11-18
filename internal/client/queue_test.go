package client

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

func testQueue(t *testing.T) *Queue {
	t.Helper()
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	return QueueFor([]*config.Runner{{Name: "box"}})
}

func join(t *testing.T, q *Queue, priority int, exclusive bool) *Ticket {
	t.Helper()
	tk, err := q.Join(priority, exclusive, "job")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tk.Leave)
	return tk
}

// holdLock takes queue.lock as a stopped process would, until the test
// ends or it is released.
func holdLock(t *testing.T, q *Queue) (unlock func()) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(q.dir, "queue.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	unlock = func() { once.Do(func() { f.Close() }) }
	t.Cleanup(unlock)
	return unlock
}

func expireSnapshot(t *testing.T) { shorten(t, &snapshotTTL, 0) }

// Hundreds of waiting tickets joining and polling at once are all served:
// routine contention must not look like a stopped lock holder (#272).
func TestQueueBurstServesEveryLookup(t *testing.T) {
	const n, late = 400, 20
	q := testQueue(t)
	tickets := make([]*Ticket, n+late)
	var wg sync.WaitGroup
	burst := func(from, to int, do func(i int)) {
		start := make(chan struct{})
		for i := from; i < to; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				do(i)
			}()
		}
		close(start)
	}
	joinErrs := make(chan error, n+late)
	joinOne := func(priority int) func(int) {
		return func(i int) {
			tk, err := q.Join(priority, false, fmt.Sprintf("job %d", i))
			if err != nil {
				joinErrs <- err
				return
			}
			tickets[i] = tk
		}
	}
	burst(0, n, func(i int) { joinOne(i % 3)(i) })
	wg.Wait()
	for _, tk := range tickets[:n] {
		if tk != nil {
			t.Cleanup(tk.Leave)
		}
	}
	select {
	case err := <-joinErrs:
		t.Fatalf("join failed under a burst: %v", err)
	default:
	}

	// Every ticket polls at once while a late, lowest-priority cohort joins.
	states := make([]QueueState, n)
	began := time.Now()
	burst(0, n, func(i int) { states[i] = q.State(tickets[i]) })
	burst(n, n+late, joinOne(-1))
	wg.Wait()
	took := time.Since(began)
	close(joinErrs)
	for _, tk := range tickets[n:] {
		if tk != nil {
			t.Cleanup(tk.Leave)
		}
	}
	for err := range joinErrs {
		t.Fatalf("join failed under a burst: %v", err)
	}

	seen := map[int]int{}
	missed := 0
	for i, st := range states {
		if st.Position >= n || st.Total < n || st.Total > n+late {
			missed++
			continue
		}
		if j, dup := seen[st.Position]; dup {
			t.Errorf("tickets %d and %d both at position %d", j, i, st.Position)
		}
		seen[st.Position] = i
	}
	if missed > 0 {
		t.Fatalf("%d of %d lookups missed their position (took %s)", missed, n, took)
	}
	// Positions follow priority (higher first), then arrival.
	for p := 1; p < n; p++ {
		a, b := tickets[seen[p-1]].tk, tickets[seen[p]].tk
		if a.Priority < b.Priority || (a.Priority == b.Priority && a.Enqueued > b.Enqueued) {
			t.Fatalf("position %d (p=%d t=%d) before position %d (p=%d t=%d)", p-1, a.Priority, a.Enqueued, p, b.Priority, b.Enqueued)
		}
	}
	// Once settled, the late cohort sits behind everyone else.
	for i := n; i < n+late; i++ {
		st := q.State(tickets[i])
		if st.Position < n || st.Position >= n+late || st.Total != n+late {
			t.Fatalf("late ticket %d: %+v, want a position in [%d,%d) of %d", i, st, n, n+late, n+late)
		}
	}
	t.Logf("%d lookups (and %d joins) in %s", n, late, took)
}

func TestQueueOrderAndExclusiveHead(t *testing.T) {
	q := testQueue(t)
	low := join(t, q, 0, false)
	excl := join(t, q, 5, true)
	high := join(t, q, 9, false)
	mid := join(t, q, 5, false) // same priority as excl, later
	want := []struct {
		tk   *Ticket
		pos  int
		head bool
	}{{high, 0, false}, {excl, 1, false}, {mid, 2, true}, {low, 3, true}}
	for _, w := range want {
		st := q.State(w.tk)
		if st.Position != w.pos || st.Total != 4 || st.ExclusiveHead != w.head {
			t.Fatalf("got %+v, want position %d of 4, exclusive ahead %v", st, w.pos, w.head)
		}
	}
}

// A warm snapshot never hides a newcomer from the next lookup: a
// higher-priority exclusive ticket stops everyone behind it at once.
func TestQueueSnapshotSeesNewcomer(t *testing.T) {
	shorten(t, &snapshotTTL, time.Hour)
	q := testQueue(t)
	a := join(t, q, 0, false)
	b := join(t, q, 0, false)
	if st := q.State(a); st.Position != 0 || st.Total != 2 {
		t.Fatalf("a: %+v", st)
	}
	x := join(t, q, 9, true)
	if st := q.State(b); st.Position != 2 || st.Total != 3 || !st.ExclusiveHead {
		t.Fatalf("b after an exclusive newcomer: %+v", st)
	}
	if st := q.State(x); st.Position != 0 {
		t.Fatalf("newcomer: %+v", st)
	}
}

// A ticket leaving while another joins (the count unchanged) is seen too.
func TestQueueSnapshotSeesSwap(t *testing.T) {
	shorten(t, &snapshotTTL, time.Hour)
	q := testQueue(t)
	gone := join(t, q, 0, false)
	b := join(t, q, 0, false)
	if st := q.State(b); st.Position != 1 || st.Total != 2 {
		t.Fatalf("b: %+v", st)
	}
	gone.Leave()
	join(t, q, 9, true)
	if st := q.State(b); st.Position != 1 || st.Total != 2 || !st.ExclusiveHead {
		t.Fatalf("b after a swap: %+v", st)
	}
}

// A ticket that joins mid-scan is seen even when the one it overtakes
// leaves before the scan reaches it: the scan's answer was true at one
// moment, never a caller at the head while an exclusive ticket was ahead.
func TestQueueScanSeesMidScanJoin(t *testing.T) {
	q := testQueue(t)
	x := join(t, q, 5, true)
	b := join(t, q, 0, false)
	xName := filepath.Base(x.path)
	var once sync.Once
	beforeInspect = func(name string) {
		if name == xName {
			once.Do(func() {
				join(t, q, 9, true)
				x.Leave()
			})
		}
	}
	t.Cleanup(func() { beforeInspect = nil })
	if st := q.State(b); st.Position != 1 || st.Total != 2 || !st.ExclusiveHead {
		t.Fatalf("b: %+v, want behind the exclusive newcomer", st)
	}
}

// The head leaving moves everyone up on their next lookup, snapshot or not.
func TestQueueSnapshotSeesDeparture(t *testing.T) {
	shorten(t, &snapshotTTL, time.Hour)
	q := testQueue(t)
	head := join(t, q, 0, false)
	next := join(t, q, 0, false)
	if st := q.State(next); st.Position != 1 {
		t.Fatalf("next: %+v", st)
	}
	head.Leave()
	if st := q.State(next); st.Position != 0 || st.Total != 1 {
		t.Fatalf("next after the head left: %+v", st)
	}
}

// A ticket whose process died (its flock released) is swept.
func TestQueueSweepsDeadTicket(t *testing.T) {
	q := testQueue(t)
	dead := join(t, q, 9, false)
	alive := join(t, q, 0, false)
	if st := q.State(alive); st.Position != 1 {
		t.Fatalf("alive: %+v", st)
	}
	dead.f.Close() // the process died; its file stays
	dead.f = nil
	expireSnapshot(t)
	if st := q.State(alive); st.Position != 0 || st.Total != 1 {
		t.Fatalf("alive after the head died: %+v", st)
	}
	if _, err := os.Stat(dead.path); !os.IsNotExist(err) {
		t.Fatalf("dead ticket not swept: %v", err)
	}
}

// A stopped holder's ticket is skipped but kept, and counts again as soon
// as it resumes, even while a snapshot that skipped it is fresh.
func TestQueueStaleTicketSkippedThenRestored(t *testing.T) {
	shorten(t, &snapshotTTL, time.Hour)
	q := testQueue(t)
	stopped := join(t, q, 9, false)
	other := join(t, q, 0, false)
	old := time.Now().Add(-2 * staleAfter)
	if err := os.Chtimes(stopped.path, old, old); err != nil {
		t.Fatal(err)
	}
	if st := q.State(other); st.Position != 0 || st.Total != 1 {
		t.Fatalf("other while the head is stopped: %+v", st)
	}
	if _, err := os.Stat(stopped.path); err != nil {
		t.Fatalf("stopped ticket removed: %v", err)
	}
	// While it stays stopped the snapshot still answers, with no rescan.
	unlock := holdLock(t, q)
	if st := q.State(other); st.Position != 0 || st.Total != 1 {
		t.Fatalf("other from the warm snapshot: %+v", st)
	}
	unlock()
	stopped.Touch() // resumed
	if st := q.State(other); st.Position != 1 || st.Total != 2 {
		t.Fatalf("other after the head resumed: %+v", st)
	}
}

// A lookup is answered from a fresh snapshot while queue.lock is held,
// fails closed once nothing fresh is published, and Join never needs it.
func TestQueueLockHeld(t *testing.T) {
	shorten(t, &queueLockWait, 200*time.Millisecond)
	shorten(t, &snapshotTTL, time.Hour)
	q := testQueue(t)
	a := join(t, q, 0, false)
	if st := q.State(a); st.Position != 0 {
		t.Fatalf("a: %+v", st)
	}
	holdLock(t, q)
	if st := q.State(a); st.Position != 0 || st.Total != 1 {
		t.Fatalf("warm lookup under a held lock: %+v", st)
	}
	b := join(t, q, 0, false) // no lock needed; it also changes the queue
	start := time.Now()
	if st := q.State(b); st.Position != unknownPosition {
		t.Fatalf("lookup with a stopped lock holder and no snapshot: %+v", st)
	}
	if d := time.Since(start); d < queueLockWait || d > queueLockWait+10*time.Second {
		t.Fatalf("gave up after %s, want about %s", d, queueLockWait)
	}
}

// A lookup whose acquire runs out of queue time stops waiting for a stopped
// lock holder then, not queueLockWait later (#295).
func TestQueueLockWaitEndsWithContext(t *testing.T) {
	shorten(t, &queueLockWait, time.Hour)
	q := testQueue(t)
	a := join(t, q, 0, false)
	holdLock(t, q)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if st := q.StateCtx(ctx, a); st.Position != unknownPosition {
		t.Fatalf("lookup with no snapshot under a held lock: %+v", st)
	}
	if d := time.Since(start); ctx.Err() == nil || d > 5*time.Second {
		t.Fatalf("gave up after %s (context: %v), want when its context ended", d, ctx.Err())
	}
}

// A waiter is answered by a snapshot published while it waits, without
// ever taking the lock.
func TestQueueWaiterUsesPublishedSnapshot(t *testing.T) {
	shorten(t, &queueLockWait, 3*time.Second)
	q := testQueue(t)
	join(t, q, 0, false)
	b := join(t, q, 0, false)
	holdLock(t, q)
	got := make(chan QueueState, 1)
	go func() { got <- q.State(b) }()
	// The lock holder's scans, until one begins after b asked. State gives
	// up within queueLockWait, so the worker always ends before the test.
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case st := <-got:
			if st.Position != 1 || st.Total != 2 {
				t.Fatalf("b: %+v", st)
			}
			return
		case <-tick.C:
			if snap, ok := q.scan(); ok {
				q.publish(snap)
			}
		}
	}
}

// Which published snapshots answer a lookup: a recent scan that began
// after it asked whatever has changed since; otherwise only a recent one of
// an unchanged queue, recent counting from when the scan finished.
func TestQueueCachedRules(t *testing.T) {
	shorten(t, &snapshotTTL, time.Minute)
	shorten(t, &queueLockWait, 10*time.Minute)
	q := testQueue(t)
	a := join(t, q, 0, false)
	z := join(t, q, 9, true)
	name := filepath.Base(a.path)
	whole := []snapEntry{{Name: filepath.Base(z.path), Exclusive: true}, {Name: name}} // the queue as it is
	partial := []snapEntry{{Name: name}}                                               // z joined after it
	now := time.Now()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	cases := []struct {
		what        string
		live        []snapEntry
		start, done time.Time
		asked       time.Time
		ok          bool
	}{
		{"post-ask, recent, changed since", partial, ago(2 * time.Second), ago(time.Second), ago(3 * time.Second), true},
		{"post-ask, recent but a slow scan, changed since", partial, ago(time.Hour), ago(time.Second), ago(3 * time.Hour), false},
		{"post-ask, read late, changed since", partial, ago(3 * time.Minute), ago(2 * time.Minute), ago(4 * time.Minute), false},
		{"post-ask, read late, unchanged", whole, ago(3 * time.Minute), ago(2 * time.Minute), ago(4 * time.Minute), true},
		{"post-ask, older than the lock wait", whole, ago(time.Hour), ago(11 * time.Minute), ago(2 * time.Hour), false},
		{"pre-ask, changed since", partial, ago(time.Hour), ago(time.Second), ago(30 * time.Minute), false},
		{"pre-ask, slow scan just finished, unchanged", whole, ago(time.Hour), ago(time.Second), ago(30 * time.Minute), true},
		{"pre-ask, expired, unchanged", whole, ago(3 * time.Minute), ago(2 * time.Minute), ago(time.Minute), false},
	}
	for _, c := range cases {
		q.publish(snapshot{Start: c.start.UnixNano(), Done: c.done.UnixNano(), Live: c.live})
		st, ok := q.cached(name, c.asked, new(os.FileInfo))
		if ok != c.ok || ok && st.Position != len(c.live)-1 {
			t.Errorf("%s: answered %v %+v, want %v", c.what, ok, st, c.ok)
		}
	}
	// A lookup doesn't decode a snapshot it found wanting again, even once
	// it would do (it waits for the next one instead).
	q.publish(snapshot{Start: ago(time.Hour).UnixNano(), Done: now.UnixNano(), Live: whole[1:]})
	looked := new(os.FileInfo)
	if _, ok := q.cached(name, ago(30*time.Minute), looked); ok {
		t.Fatal("a changed queue answered")
	}
	z.Leave()
	if _, ok := q.cached(name, ago(30*time.Minute), looked); ok {
		t.Fatal("a snapshot already found wanting was decoded again")
	}
	if _, ok := q.cached(name, ago(30*time.Minute), new(os.FileInfo)); !ok {
		t.Fatal("the snapshot of a queue now unchanged didn't answer a new lookup")
	}
}

// A ticket that can't be inspected makes the scan inconclusive: nobody is
// admitted on it, and it isn't published.
func TestQueueInconclusiveScan(t *testing.T) {
	q := testQueue(t)
	a := join(t, q, 0, false)
	head := join(t, q, 9, true)
	if os.Geteuid() == 0 {
		t.Skip("root opens any file")
	}
	if err := os.Chmod(head.path, 0); err != nil { // can't open it
		t.Fatal(err)
	}
	expireSnapshot(t)
	if st := q.State(a); st.Position != unknownPosition {
		t.Fatalf("a with an unreadable head: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(q.dir, snapshotName)); !os.IsNotExist(err) {
		t.Fatalf("inconclusive scan published: %v", err)
	}
}

// One scan serves a whole burst of lookups on an unchanging queue.
func TestQueueBurstScansOnce(t *testing.T) {
	shorten(t, &snapshotTTL, time.Hour)
	const n = 100
	q := testQueue(t)
	tickets := make([]*Ticket, n)
	for i := range tickets {
		tickets[i] = join(t, q, 0, false)
	}
	before := queueScans.Load()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, tk := range tickets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if st := q.State(tk); st.Position >= n {
				t.Error("lookup missed its position")
			}
		}()
	}
	close(start)
	wg.Wait()
	if scans := queueScans.Load() - before; scans != 1 {
		t.Fatalf("%d scans for %d lookups, want 1", scans, n)
	}
}

func TestQueueIgnoresBadSnapshot(t *testing.T) {
	shorten(t, &snapshotTTL, time.Hour)
	q := testQueue(t)
	a := join(t, q, 0, false)
	name := filepath.Base(a.path)
	// Each would put a phantom exclusive ticket ahead of a, if believed.
	now, later := time.Now().UnixNano(), time.Now().Add(time.Hour).UnixNano()
	future := fmt.Sprintf(`{"start":%d,"done":%d,"live":[{"n":"x.ticket","x":true},{"n":%q}]}`, later, later, name)
	badType := fmt.Sprintf(`{"start":%d,"done":%d,"live":[{"n":"x.ticket","x":true},{"n":%q}],"stale":7}`, now, now, name)
	for _, body := range []string{"{not json", future, badType} {
		if err := os.WriteFile(filepath.Join(q.dir, snapshotName), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if st := q.State(a); st.Position != 0 || st.Total != 1 || st.ExclusiveHead {
			t.Fatalf("with snapshot %q: %+v", body, st)
		}
	}
}

// A ticket abandoned mid-Join is swept once old; a fresh one is left for
// its writer.
func TestQueueSweepsAbandonedJoin(t *testing.T) {
	q := testQueue(t)
	a := join(t, q, 0, false)
	oldTmp := filepath.Join(q.dir, "1-dead"+joiningExt)
	newTmp := filepath.Join(q.dir, "2-busy"+joiningExt)
	heldTmp := filepath.Join(q.dir, "3-held"+joiningExt) // old, but its writer is alive
	for _, p := range []string{oldTmp, newTmp, heldTmp} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleAfter)
	for _, p := range []string{oldTmp, heldTmp} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	hf, err := os.OpenFile(heldTmp, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer hf.Close()
	if err := syscall.Flock(int(hf.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	expireSnapshot(t)
	if st := q.State(a); st.Position != 0 || st.Total != 1 {
		t.Fatalf("a: %+v", st)
	}
	if _, err := os.Stat(oldTmp); !os.IsNotExist(err) {
		t.Fatalf("abandoned join not swept: %v", err)
	}
	for _, p := range []string{newTmp, heldTmp} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("join in progress swept: %v", err)
		}
	}
}

// BenchmarkQueueBurst measures a burst of lookups at hundreds of tickets.
func BenchmarkQueueBurst(b *testing.B) {
	b.Setenv("SPOTEXEC_STATE_DIR", b.TempDir())
	q := QueueFor([]*config.Runner{{Name: "box"}})
	const n = 256
	tickets := make([]*Ticket, n)
	for i := range tickets {
		tk, err := q.Join(i%3, false, "job")
		if err != nil {
			b.Fatal(err)
		}
		defer tk.Leave()
		tickets[i] = tk
	}
	b.ResetTimer()
	for range b.N {
		var wg sync.WaitGroup
		for _, tk := range tickets {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if st := q.State(tk); st.Position >= n {
					b.Error("lookup missed its position")
				}
			}()
		}
		wg.Wait()
	}
}
