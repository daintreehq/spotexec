package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	t         *testing.T
	mu        sync.Mutex
	owner     string
	ids       []string        // inventory, in listing order
	live      map[string]bool // not yet terminated
	failCall  map[int]bool    // TerminateInstances calls (1-based) to refuse
	stopped   bool            // list every instance as stopped
	termCalls int
	terms     [][]string // IDs of each accepted TerminateInstances
	pages     int
}

func newFakeBulkEC2(t *testing.T, n int) *fakeBulkEC2 {
	f := &fakeBulkEC2{t: t, live: map[string]bool{}, failCall: map[int]bool{}}
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
			f.termCalls++
			var ids []string
			for i := 1; p.Get("InstanceId."+strconv.Itoa(i)) != ""; i++ {
				ids = append(ids, p.Get("InstanceId."+strconv.Itoa(i)))
			}
			if len(ids) > 1000 {
				ec2Err(w, 400, "InvalidParameterValue")
				return
			}
			if f.failCall[f.termCalls] {
				ec2Err(w, 400, "UnauthorizedOperation")
				return
			}
			for _, id := range ids {
				if !f.live[id] {
					f.t.Errorf("terminated %s, which isn't live", id)
				}
				delete(f.live, id)
			}
			f.terms = append(f.terms, ids)
			fmt.Fprint(w, `<TerminateInstancesResponse/>`)
		default:
			f.t.Errorf("unexpected %s", p.Get("Action"))
			ec2Err(w, 400, "InvalidAction")
		}
	})
}

func (f *fakeBulkEC2) describe(w http.ResponseWriter, p url.Values) {
	// The listing must keep both ownership filters.
	if p.Get("Filter.1.Name") != "tag:"+tagRunner || p.Get("Filter.1.Value.1") != "s" ||
		p.Get("Filter.2.Name") != "tag:"+tagOwner || p.Get("Filter.2.Value.1") != f.owner {
		f.t.Errorf("listing without the runner and owner filters: %v", p)
	}
	f.pages++
	size, _ := strconv.Atoi(p.Get("MaxResults"))
	if size <= 0 || size > 1000 {
		size = 1000
	}
	start, _ := strconv.Atoi(p.Get("NextToken"))
	var live []string
	for _, id := range f.ids {
		if f.live[id] {
			live = append(live, id)
		}
	}
	end := min(start+size, len(live))
	st := "running"
	if f.stopped {
		st = "stopped"
	}
	fmt.Fprint(w, `<DescribeInstancesResponse><reservationSet><item><instancesSet>`)
	for _, id := range live[start:end] {
		fmt.Fprintf(w, `<item><instanceId>%s</instanceId><instanceType>c7a.2xlarge</instanceType><instanceState><name>%s</name></instanceState><tagSet><item><key>%s</key><value>s</value></item><item><key>%s</key><value>%s</value></item></tagSet></item>`,
			id, st, tagRunner, tagOwner, f.owner)
	}
	fmt.Fprint(w, `</instancesSet></item></reservationSet>`)
	if end < len(live) {
		fmt.Fprintf(w, `<nextToken>%d</nextToken>`, end)
	}
	fmt.Fprint(w, `</DescribeInstancesResponse>`)
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
	r := &config.Runner{Name: "s", Type: config.TypeAWSSpot, Region: "r-one-1",
		InstanceTypes: []string{"c7a.2xlarge"}, Arch: "amd64", User: "ubuntu", VolumeGB: 64,
		SSHCIDR: "0.0.0.0/0", MaxInstances: 4, MaxLifetime: config.Duration{Duration: 2 * time.Hour},
		BootTimeout: config.Duration{Duration: time.Minute}, IdleTimeout: config.Duration{Duration: time.Minute}}
	a := newAWSSpot(r, &awsCredResolver{fixed: &awsCreds{AccessKeyID: "AKID", SecretAccessKey: "secret"}})
	a.api.endpoints = map[string]string{"ec2": srv.URL + "/r-one-1/ec2"}
	return a
}

func batchSizes(terms [][]string) []int {
	var out []int
	for _, b := range terms {
		out = append(out, len(b))
	}
	return out
}

func TestDownBatchesTerminations(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want []int
	}{
		{1, []int{1}},
		{1000, []int{1000}},
		{1001, []int{1000, 1}},
		{3500, []int{1000, 1000, 1000, 500}},
	} {
		t.Run(strconv.Itoa(tc.n), func(t *testing.T) {
			f := newFakeBulkEC2(t, tc.n)
			a := newBulkEnv(t, f)
			var logged int
			n, err := a.downLocked(context.Background(), func(format string, _ ...any) {
				if strings.HasPrefix(format, "terminating ") {
					logged++
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			if n != tc.n || logged != tc.n {
				t.Fatalf("terminated %d, logged %d, want %d", n, logged, tc.n)
			}
			if got := batchSizes(f.terms); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("batches %v, want %v", got, tc.want)
			}
			if wantPages := (tc.n + 99) / 100; f.pages != wantPages {
				t.Fatalf("listed %d pages, want %d", f.pages, wantPages)
			}
			if len(f.live) != 0 {
				t.Fatalf("%d instances left", len(f.live))
			}
		})
	}
}

func TestDownReportsFailedBatchAndRetries(t *testing.T) {
	f := newFakeBulkEC2(t, 2500)
	f.failCall[2] = true // the second of three batches is refused
	a := newBulkEnv(t, f)
	var logged []string
	n, err := a.downLocked(context.Background(), func(format string, args ...any) {
		if strings.HasPrefix(format, "terminating ") {
			logged = append(logged, fmt.Sprint(args[0]))
		}
	})
	if err == nil {
		t.Fatal("want an error for the refused batch")
	}
	for _, want := range []string{"batch 2 of 3", "1000 of 2500", "UnauthorizedOperation"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if n != 1500 || len(logged) != 1500 {
		t.Fatalf("terminated %d, logged %d, want 1500", n, len(logged))
	}
	if got := batchSizes(f.terms); fmt.Sprint(got) != "[1000 500]" {
		t.Fatalf("accepted batches %v, want [1000 500]", got)
	}
	// Only the refused batch's instances are left, and only accepted ones
	// were reported.
	for _, id := range f.ids[1000:2000] {
		if !f.live[id] {
			t.Fatalf("%s from the refused batch was terminated", id)
		}
	}
	for _, id := range logged {
		if f.live[id] {
			t.Fatalf("reported %s, which is still live", id)
		}
	}
	if len(f.live) != 1000 {
		t.Fatalf("%d instances left, want 1000", len(f.live))
	}

	// Running down again finishes the job, touching only what's left.
	n, err = a.downLocked(context.Background(), func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1000 || len(f.live) != 0 {
		t.Fatalf("retry terminated %d, %d left", n, len(f.live))
	}
	if got := batchSizes(f.terms); fmt.Sprint(got) != "[1000 500 1000]" {
		t.Fatalf("accepted batches %v, want [1000 500 1000]", got)
	}
}
func TestTerminateBatchesIDs(t *testing.T) {
	f := newFakeBulkEC2(t, 1001)
	f.stopped = true
	a := newBulkEnv(t, f)
	done, err := a.terminate(context.Background(), f.ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1001 {
		t.Fatalf("terminated %d, want 1001", len(done))
	}
	if got := batchSizes(f.terms); fmt.Sprint(got) != "[1000 1]" {
		t.Fatalf("batches %v, want [1000 1]", got)
	}
}
