package provider

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// holdSSOLock takes the native SSO cache lock for key the way another
// process would, and returns its release.
func holdSSOLock(t *testing.T, home, key string) func() {
	t.Helper()
	f, err := os.OpenFile(ssoCachePath(home, key)+".spotexec-lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	var once atomic.Bool
	release := func() {
		if once.CompareAndSwap(false, true) {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			f.Close()
		}
	}
	t.Cleanup(release)
	return release
}

// waitReturn runs fn and fails the test if it hasn't returned within d.
func waitReturn[T any](t *testing.T, d time.Duration, fn func() T) T {
	t.Helper()
	ch := make(chan T, 1)
	go func() { ch <- fn() }()
	select {
	case v := <-ch:
		return v
	case <-time.After(d):
		t.Fatalf("still blocked after %v", d)
		panic("unreachable")
	}
}

// A lookup whose deadline passes while another process holds the token cache
// lock gives up with the context's error instead of waiting for the holder;
// once the holder lets go, lookups go through again.
func TestSSOTokenLockHonorsContext(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeSSOCache(t, home, "corp", freshToken("tok"))
	noNetwork(t)
	release := holdSSOLock(t, home, "corp")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := waitReturn(t, 2*time.Second, func() error {
		_, err := ssoToken(ctx, "corp", "us-west-2", "dev")
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context deadline", err)
	}
	release()
	if tok, err := ssoToken(context.Background(), "corp", "us-west-2", "dev"); err != nil || tok != "tok" {
		t.Fatalf("after release: %q %v", tok, err)
	}
}

// While one resolution is stuck behind a live lock holder, a second caller
// with a deadline returns when its deadline passes, and callers without one
// wait for the first and share its result rather than resolving again.
func TestCredResolverWaiterHonorsContext(t *testing.T) {
	_, home, key := ssoProfile(t, "us-east-1", false)
	writeSSOCache(t, home, key, freshToken("tok"))
	t.Setenv("PATH", t.TempDir()) // no AWS CLI: the native SSO path
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(home, "no-credentials"))
	var calls atomic.Int32
	stubNetwork(t, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(`{"roleCredentials":{"accessKeyId":"AKIDSSO","secretAccessKey":"sek","sessionToken":"st","expiration":4102444800000}}`), nil
	})
	release := holdSSOLock(t, home, key)
	r := &awsCredResolver{profile: "dev"}

	type result struct {
		c   awsCreds
		err error
	}
	first := make(chan result, 1)
	go func() { c, err := r.get(context.Background()); first <- result{c, err} }()
	waitInFlight(t, r)
	second := make(chan result, 1)
	go func() { c, err := r.get(context.Background()); second <- result{c, err} }()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := waitReturn(t, 2*time.Second, func() error { _, err := r.get(ctx); return err })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context deadline", err)
	}

	release()
	for _, ch := range []chan result{first, second} {
		select {
		case res := <-ch:
			if res.err != nil || res.c.AccessKeyID != "AKIDSSO" {
				t.Fatalf("waiting caller: %+v %v", res.c, res.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("caller still blocked after the lock was released")
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("%d role credential lookups, want 1 shared by every caller", n)
	}
}

// waitInFlight waits until r has a resolution under way.
func waitInFlight(t *testing.T, r *awsCredResolver) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		busy := r.resolving != nil
		r.mu.Unlock()
		if busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no resolution started")
}

// Two lookups racing to refresh an expired token refresh it once: the second
// waits for the lock and then reads the token the first saved.
func TestSSOTokenRefreshSerialized(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeSSOCache(t, home, "corp", expiredRefreshable())
	var posts atomic.Int32
	stubNetwork(t, func(r *http.Request) (*http.Response, error) {
		posts.Add(1)
		time.Sleep(50 * time.Millisecond) // hold the lock while the other waits
		return jsonResponse(`{"accessToken":"new","expiresIn":3600,"refreshToken":"rotated"}`), nil
	})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			tok, err := ssoToken(context.Background(), "corp", "us-west-2", "dev")
			if err == nil && tok != "new" {
				err = errors.New("token " + tok)
			}
			errs <- err
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := posts.Load(); n != 1 {
		t.Errorf("%d refreshes, want 1", n)
	}
}

// nativeSSOResolver sets up profile "dev" resolving through the native SSO
// path with a fresh cached token, and returns its cache key and HOME.
func nativeSSOResolver(t *testing.T) (home, key string) {
	t.Helper()
	_, home, key = ssoProfile(t, "us-east-1", false)
	writeSSOCache(t, home, key, freshToken("tok"))
	t.Setenv("PATH", t.TempDir()) // no AWS CLI
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(home, "no-credentials"))
	return home, key
}

func roleCreds(id string) *http.Response {
	return jsonResponse(`{"roleCredentials":{"accessKeyId":"` + id + `","secretAccessKey":"sek","sessionToken":"st","expiration":4102444800000}}`)
}

// When the caller resolving for everyone is cancelled, a waiter that is
// still live resolves under its own context instead of failing with the
// other caller's error.
func TestCredResolverWaiterOutlivesCancelledLeader(t *testing.T) {
	home, key := nativeSSOResolver(t)
	var calls atomic.Int32
	stubNetwork(t, func(r *http.Request) (*http.Response, error) { calls.Add(1); return roleCreds("AKIDSSO"), nil })
	release := holdSSOLock(t, home, key)
	r := &awsCredResolver{profile: "dev"}
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leader := make(chan error, 1)
	go func() { _, err := r.get(leaderCtx); leader <- err }()
	waitInFlight(t, r)
	waiter := make(chan error, 1)
	go func() {
		c, err := r.get(context.Background())
		if err == nil && c.AccessKeyID != "AKIDSSO" {
			err = errors.New("got " + c.AccessKeyID)
		}
		waiter <- err
	}()
	cancelLeader()
	if err := waitReturn(t, 2*time.Second, func() error { return <-leader }); !errors.Is(err, context.Canceled) {
		t.Fatalf("leader: %v, want its cancellation", err)
	}
	release()
	if err := waitReturn(t, 5*time.Second, func() error { return <-waiter }); err != nil {
		t.Fatalf("waiter: %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("%d role credential lookups, want 1", n)
	}
}

// Credentials resolved while an invalidation arrives may be the ones that
// were just rejected: they are dropped and resolved again.
func TestCredResolverInvalidateDropsInFlightResult(t *testing.T) {
	nativeSSOResolver(t)
	entered, gate := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	stubNetwork(t, func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-gate
			return roleCreds("AKOLD"), nil
		}
		return roleCreds("AKNEW"), nil
	})
	r := &awsCredResolver{profile: "dev"}
	got := make(chan awsCreds, 1)
	go func() { c, _ := r.get(context.Background()); got <- c }()
	<-entered
	r.invalidate()
	close(gate)
	if c := waitReturn(t, 5*time.Second, func() awsCreds { return <-got }); c.AccessKeyID != "AKNEW" {
		t.Fatalf("got %q, want the credentials resolved after the invalidation", c.AccessKeyID)
	}
	if c, err := r.get(context.Background()); err != nil || c.AccessKeyID != "AKNEW" {
		t.Fatalf("cached: %q %v", c.AccessKeyID, err)
	}
}
