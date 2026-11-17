package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// fakeBulkEC2 holds a large fleet in one region: DescribeInstances pages it
// by MaxResults, and TerminateInstances enforces EC2's limit of 1,000 IDs
// per request.
type fakeBulkEC2 struct {
	t        *testing.T
	mu       sync.Mutex
	owner    string
	ids      []string        // inventory, in listing order
	live     map[string]bool // not yet terminated
	state    string          // state every instance is listed in (default running)
	decoys   bool            // the first page also holds another owner's and another runner's instance
	failCall map[int]bool    // TerminateInstances requests (1-based) to refuse
	throttle int             // after the first accepted batch, throttle this many requests
	// cancelOnAccept, if set, is called once a batch is accepted, and that
	// request's response is withheld (EC2 took it, the answer was lost).
	cancelOnAccept context.CancelFunc

	termCalls    int
	throttled    int
	lastThrottle time.Time     // when the last throttled request was answered
	refillGap    time.Duration // from the last throttle to the next accepted request
	attempts     []int         // size of every TerminateInstances request
	terms        [][]string    // IDs of each accepted request
	pages        int
	srv          *httptest.Server
}

func newFakeBulkEC2(t *testing.T, n int) *fakeBulkEC2 {
	f := &fakeBulkEC2{t: t, live: map[string]bool{}, failCall: map[int]bool{}, state: "running"}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("i-%05d", i)
		f.ids = append(f.ids, id)
		f.live[id] = true
	}
	return f
}

func (f *fakeBulkEC2) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p, _ := url.ParseQuery(string(body))
		f.mu.Lock()
		defer f.mu.Unlock()
		switch p.Get("Action") {
		case "DescribeInstances":
			f.describe(w, p)
		case "TerminateInstances":
			f.terminate(w, r, p)
		default:
			f.t.Errorf("unexpected %s", p.Get("Action"))
			ec2Err(w, 400, "InvalidAction")
		}
	})
}

func (f *fakeBulkEC2) terminate(w http.ResponseWriter, r *http.Request, p url.Values) {
	f.termCalls++
	var ids []string
	for k := range p {
		if strings.HasPrefix(k, "InstanceId.") {
			ids = append(ids, "")
		}
	}
	for i := range ids {
		ids[i] = p.Get("InstanceId." + strconv.Itoa(i+1))
		if ids[i] == "" {
			f.t.Errorf("InstanceId.%d missing from a request of %d", i+1, len(ids))
		}
	}
	f.attempts = append(f.attempts, len(ids))
	if len(ids) > 1000 {
		ec2Err(w, 400, "InvalidParameterValue")
		return
	}
	if f.failCall[f.termCalls] {
		ec2Err(w, 400, "UnauthorizedOperation")
		return
	}
	if len(f.terms) > 0 && f.throttled < f.throttle {
		f.throttled++
		ec2Err(w, 503, "RequestLimitExceeded")
		f.lastThrottle = time.Now()
		return
	}
	if !f.lastThrottle.IsZero() && f.refillGap == 0 {
		f.refillGap = time.Since(f.lastThrottle)
	}
	for _, id := range ids {
		if !f.live[id] {
			f.t.Errorf("terminated %s, which isn't live", id)
		}
		delete(f.live, id)
	}
	f.terms = append(f.terms, ids)
	if cancel := f.cancelOnAccept; cancel != nil {
		f.cancelOnAccept = nil
		cancel()
		f.mu.Unlock()
		<-r.Context().Done()
		f.mu.Lock()
		return
	}
	fmt.Fprint(w, `<TerminateInstancesResponse/>`)
}

func (f *fakeBulkEC2) describe(w http.ResponseWriter, p url.Values) {
	// The listing must keep both ownership filters.
	if p.Get("Filter.1.Name") != "tag:"+tagRunner || p.Get("Filter.1.Value.1") != "s" ||
		p.Get("Filter.2.Name") != "tag:"+tagOwner || p.Get("Filter.2.Value.1") != f.owner {
		f.t.Errorf("listing without the runner and owner filters: %v", p)
	}
	f.pages++
	wanted := false
	for i := 1; p.Get("Filter.3.Value."+strconv.Itoa(i)) != ""; i++ {
		wanted = wanted || p.Get("Filter.3.Value."+strconv.Itoa(i)) == f.state
	}
	size, _ := strconv.Atoi(p.Get("MaxResults"))
	if size <= 0 || size > 1000 {
		size = 1000
	}
	start, _ := strconv.Atoi(p.Get("NextToken"))
	var live []string
	for _, id := range f.ids {
		if f.live[id] && wanted {
			live = append(live, id)
		}
	}
	end := min(start+size, len(live))
	fmt.Fprint(w, `<DescribeInstancesResponse><reservationSet><item><instancesSet>`)
	if f.decoys && start == 0 {
		// Returned despite the filters (as a stale listing might): the
		// tags still rule them out.
		fmt.Fprintf(w, `<item><instanceId>i-other-owner</instanceId><instanceState><name>running</name></instanceState><tagSet><item><key>%s</key><value>s</value></item><item><key>%s</key><value>someone-else</value></item></tagSet></item>`, tagRunner, tagOwner)
		fmt.Fprintf(w, `<item><instanceId>i-other-runner</instanceId><instanceState><name>running</name></instanceState><tagSet><item><key>%s</key><value>other</value></item><item><key>%s</key><value>%s</value></item></tagSet></item>`, tagRunner, tagOwner, f.owner)
	}
	for _, id := range live[start:end] {
		fmt.Fprintf(w, `<item><instanceId>%s</instanceId><instanceType>c7a.2xlarge</instanceType><instanceState><name>%s</name></instanceState><tagSet><item><key>%s</key><value>s</value></item><item><key>%s</key><value>%s</value></item></tagSet></item>`,
			id, f.state, tagRunner, tagOwner, f.owner)
	}
	fmt.Fprint(w, `</instancesSet></item></reservationSet>`)
	if end < len(live) {
		fmt.Fprintf(w, `<nextToken>%d</nextToken>`, end)
	}
	fmt.Fprint(w, `</DescribeInstancesResponse>`)
}

// settle waits for any handler still holding the fake, so a test reads its
// state after every request it made has been recorded.
func (f *fakeBulkEC2) settle() {
	f.mu.Lock()
	f.mu.Unlock()
}

// finish shuts the server down, waiting for every handler, even one a
// cancelled client left behind.
func (f *fakeBulkEC2) finish() {
	f.srv.Close()
	f.settle()
}

func newBulkEnv(t *testing.T, f *fakeBulkEC2) *AWSSpot {
	t.Helper()
	t.Setenv("SPOTEXEC_CONFIG_DIR", t.TempDir())
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	owner, err := ownerID()
	if err != nil {
		t.Fatal(err)
	}
	f.owner = owner
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	f.srv = srv
	r := &config.Runner{Name: "s", Type: config.TypeAWSSpot, Region: "r-one-1",
		InstanceTypes: []string{"c7a.2xlarge"}, Arch: "amd64", User: "ubuntu", VolumeGB: 64,
		SSHCIDR: "0.0.0.0/0", MaxInstances: 4, MaxLifetime: config.Duration{Duration: 2 * time.Hour},
		BootTimeout: config.Duration{Duration: time.Minute}, IdleTimeout: config.Duration{Duration: time.Minute}}
	a := newAWSSpot(r, &awsCredResolver{fixed: &awsCreds{AccessKeyID: "AKID", SecretAccessKey: "secret"}})
	a.api.endpoints = map[string]string{"ec2": srv.URL + "/r-one-1/ec2"}
	return a
}

// fastRetries shortens query's backoff and the refill wait for a test.
func fastRetries(t *testing.T, refill time.Duration) {
	oldBackoff, oldRefill := queryRetryBase, terminateRefill
	queryRetryBase, terminateRefill = time.Millisecond, refill
	t.Cleanup(func() { queryRetryBase, terminateRefill = oldBackoff, oldRefill })
}

func batchSizes(terms [][]string) []int {
	var out []int
	for _, b := range terms {
		out = append(out, len(b))
	}
	return out
}

// downLog records the IDs downLocked reports and any other lines.
type downLog struct {
	ids   []string
	other []string
}

func (d *downLog) log(format string, args ...any) {
	if strings.HasPrefix(format, "terminating ") {
		d.ids = append(d.ids, fmt.Sprint(args[0]))
		return
	}
	d.other = append(d.other, fmt.Sprintf(format, args...))
}

func TestDownBatchesTerminations(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want []int
	}{
		{0, nil},
		{1, []int{1}},
		{1000, []int{1000}},
		{1001, []int{1000, 1}},
		{3500, []int{1000, 1000, 1000, 500}},
	} {
		t.Run(strconv.Itoa(tc.n), func(t *testing.T) {
			f := newFakeBulkEC2(t, tc.n)
			f.decoys = true
			a := newBulkEnv(t, f)
			var d downLog
			n, err := a.downLocked(context.Background(), d.log)
			f.settle()
			if err != nil {
				t.Fatal(err)
			}
			if n != tc.n {
				t.Fatalf("terminated %d, want %d", n, tc.n)
			}
			// Every instance reported once, in listing order; no decoy.
			if strings.Join(d.ids, ",") != strings.Join(f.ids, ",") {
				t.Fatalf("reported %d IDs, not the %d listed", len(d.ids), len(f.ids))
			}
			if fmt.Sprint(f.attempts) != fmt.Sprint(tc.want) || fmt.Sprint(batchSizes(f.terms)) != fmt.Sprint(tc.want) {
				t.Fatalf("requests %v, accepted %v, want %v", f.attempts, batchSizes(f.terms), tc.want)
			}
			if wantPages := max(1, (tc.n+99)/100); f.pages != wantPages {
				t.Fatalf("listed %d pages, want %d", f.pages, wantPages)
			}
			if len(f.live) != 0 {
				t.Fatalf("%d instances left", len(f.live))
			}
			if tc.n == 0 && (len(d.other) != 1 || !strings.HasPrefix(d.other[0], "no running instances")) {
				t.Fatalf("logged %v", d.other)
			}
		})
	}
}

func TestDownReportsRefusedBatchesAndRetries(t *testing.T) {
	f := newFakeBulkEC2(t, 2500)
	f.failCall[2], f.failCall[3] = true, true // batches 2 (1000) and 3 (500) are refused
	a := newBulkEnv(t, f)
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, id := range f.ids {
		if err := os.WriteFile(a.knownHosts(id), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var d downLog
	n, err := a.downLocked(context.Background(), d.log)
	f.settle()
	if err == nil {
		t.Fatal("want an error for the refused batches")
	}
	for _, want := range []string{"1500 of 2500 instances not confirmed", "batch 2 of 3 (1000 instances)",
		"batch 3 of 3 (500 instances)", "UnauthorizedOperation"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if n != 1000 || strings.Join(d.ids, ",") != strings.Join(f.ids[:1000], ",") {
		t.Fatalf("terminated %d, reported %d, want the first batch", n, len(d.ids))
	}
	// Only the accepted batch is gone, and only its known_hosts files.
	for i, id := range f.ids {
		refused := i >= 1000
		if f.live[id] != refused {
			t.Fatalf("%s live=%v, want %v", id, f.live[id], refused)
		}
		_, serr := os.Stat(a.knownHosts(id))
		if refused && serr != nil || !refused && !errors.Is(serr, fs.ErrNotExist) {
			t.Fatalf("%s known_hosts: %v (refused=%v)", id, serr, refused)
		}
	}

	// Running down again finishes the job, sending only what's left.
	before := len(f.terms)
	d = downLog{}
	n, err = a.downLocked(context.Background(), d.log)
	f.settle()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1500 || len(f.live) != 0 || strings.Join(d.ids, ",") != strings.Join(f.ids[1000:], ",") {
		t.Fatalf("retry terminated %d, reported %d, %d left", n, len(d.ids), len(f.live))
	}
	var sent []string
	for _, b := range f.terms[before:] {
		sent = append(sent, b...)
	}
	if fmt.Sprint(batchSizes(f.terms[before:])) != "[1000 500]" || strings.Join(sent, ",") != strings.Join(f.ids[1000:], ",") {
		t.Fatalf("retry sent batches %v", batchSizes(f.terms[before:]))
	}
}

func TestUpTerminatesStoppedLeftoversInBatches(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refuse  int
		want    string
		wantErr string
	}{
		{"all", 0, "terminated 1001 stopped instance(s) left over for s", ""},
		{"refused", 2, "terminated 1000 stopped instance(s) left over for s", "batch 2 of 2 (1 instances)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeBulkEC2(t, 1001)
			f.state = "stopped"
			if tc.refuse > 0 {
				f.failCall[tc.refuse] = true
			}
			a := newBulkEnv(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var lines []string
			_, err := a.Up(ctx, func(format string, args ...any) {
				line := fmt.Sprintf(format, args...)
				lines = append(lines, line)
				if strings.HasPrefix(line, "terminated ") {
					cancel() // stop Up at its launch, which follows the cleanup
				}
			})
			f.settle()
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Up: %v (want it to go on to launch)", err)
			}
			if fmt.Sprint(f.attempts) != "[1000 1]" {
				t.Fatalf("requests %v, want [1000 1]", f.attempts)
			}
			all := strings.Join(lines, "\n")
			if !strings.Contains(all, tc.want) {
				t.Fatalf("log %q lacks %q", all, tc.want)
			}
			refusal := strings.Contains(all, "stopped leftovers for s: ")
			if refusal != (tc.wantErr != "") || !strings.Contains(all, tc.wantErr) {
				t.Fatalf("log %q: refusal logged=%v, want %q", all, refusal, tc.wantErr)
			}
		})
	}
}

func TestTerminateWaitsOutThrottle(t *testing.T) {
	fastRetries(t, 2*time.Millisecond)
	for _, tc := range []struct {
		n        int
		min, max time.Duration // the refill wait for the throttled second batch
	}{
		{2000, 2 * time.Second, 0},
		{1500, time.Second, 1900 * time.Millisecond}, // sized for its 500, not a full 1000
	} {
		t.Run(strconv.Itoa(tc.n), func(t *testing.T) {
			f := newFakeBulkEC2(t, tc.n)
			f.throttle = 5 // as many as query retries itself
			a := newBulkEnv(t, f)
			done, err := a.terminate(context.Background(), f.ids)
			f.settle()
			if err != nil {
				t.Fatal(err)
			}
			if len(done) != tc.n || len(f.live) != 0 || f.throttled != 5 || len(f.terms) != 2 {
				t.Fatalf("terminated %d, %d left, throttled %d, accepted %d", len(done), len(f.live), f.throttled, len(f.terms))
			}
			if gap := f.refillGap; gap < tc.min || tc.max > 0 && gap > tc.max {
				t.Fatalf("waited %v after the throttle, want the refill wait (%v to %v)", gap, tc.min, tc.max)
			}
		})
	}
}

func TestTerminateGivesUpOnPersistentThrottle(t *testing.T) {
	fastRetries(t, time.Microsecond)
	f := newFakeBulkEC2(t, 2500)
	f.throttle = 10 // the second batch, through query's retries twice
	a := newBulkEnv(t, f)
	done, err := a.terminate(context.Background(), f.ids)
	f.settle()
	if len(done) != 1500 || f.throttled != 10 || fmt.Sprint(batchSizes(f.terms)) != "[1000 500]" {
		t.Fatalf("terminated %d, throttled %d, accepted %v", len(done), f.throttled, batchSizes(f.terms))
	}
	if err == nil || !strings.Contains(err.Error(), "batch 2 of 3 (1000 instances)") || awsCode(err) != "RequestLimitExceeded" {
		t.Fatalf("error %v", err)
	}
}

func TestTerminateCancelledDuringRefill(t *testing.T) {
	fastRetries(t, time.Hour)
	f := newFakeBulkEC2(t, 2000)
	f.throttle = 100
	a := newBulkEnv(t, f)
	// Long enough for query's five quick throttled attempts, far short of
	// the hour-long refill.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	done, err := a.terminate(ctx, f.ids)
	f.finish()
	if time.Since(start) > 10*time.Second {
		t.Fatalf("took %v to notice the deadline", time.Since(start))
	}
	if len(done) != 1000 || f.throttled != 5 {
		t.Fatalf("terminated %d, throttled %d", len(done), f.throttled)
	}
	// Both: the batch was throttled, then the deadline ended the wait.
	if !errors.Is(err, context.DeadlineExceeded) || awsCode(err) != "RequestLimitExceeded" ||
		!strings.Contains(err.Error(), "batch 2 of 2") {
		t.Fatalf("error %v", err)
	}
}

func TestTerminateStopsWhenCancelled(t *testing.T) {
	f := newFakeBulkEC2(t, 2500)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancelled while the first batch is in flight: EC2 took it, but no
	// answer came back, so it isn't reported, and nothing more is sent.
	f.cancelOnAccept = cancel
	a := newBulkEnv(t, f)
	done, err := a.terminate(ctx, f.ids)
	f.finish()
	if len(done) != 0 || fmt.Sprint(f.attempts) != "[1000]" || len(f.terms) != 1 {
		t.Fatalf("reported %d; requests %v, accepted %d", len(done), f.attempts, len(f.terms))
	}
	for _, want := range []string{"2500 of 2500 instances not confirmed", "batch 1 of 3 (1000 instances)",
		"batches 2 to 3 of 3 (1500 instances) not sent"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("error %v lacks %q", err, want)
		}
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v isn't context.Canceled", err)
	}
}
