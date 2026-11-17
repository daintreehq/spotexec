package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// bootRead is what the fake EC2 endpoint answers to the nth status read.
type bootRead func(n int) (status int, body string)

const (
	bootThrottled = `<Response><Errors><Error><Code>RequestLimitExceeded</Code><Message>Request limit exceeded.</Message></Error></Errors></Response>`
	bootDenied    = `<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>You are not authorized to perform this operation.</Message></Error></Errors></Response>`
)

func bootInstance(state, ip string) string {
	return fmt.Sprintf(`<DescribeInstancesResponse><reservationSet><item><instancesSet><item><instanceId>i-boot</instanceId><instanceType>c7a.xlarge</instanceType><instanceState><name>%s</name></instanceState><ipAddress>%s</ipAddress></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`, state, ip)
}

// newBootEnv serves DescribeInstances from read, with the boot poll and
// the Query client's retry backoff shrunk so a test runs in milliseconds.
func newBootEnv(t *testing.T, bootTimeout time.Duration, read bootRead) (*AWSSpot, *atomic.Int32) {
	t.Helper()
	oldPoll, oldBase := bootPollInterval, queryRetryBase
	bootPollInterval, queryRetryBase = 5*time.Millisecond, time.Millisecond
	t.Cleanup(func() { bootPollInterval, queryRetryBase = oldPoll, oldBase })
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if a := r.Form.Get("Action"); a != "DescribeInstances" {
			t.Errorf("unexpected call %s", a)
			w.WriteHeader(400)
			return
		}
		status, body := read(int(n.Add(1)))
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	r := &config.Runner{Name: "aws", Type: "aws-spot", Region: "us-east-2", User: "ubuntu"}
	r.BootTimeout.Duration = bootTimeout
	return &AWSSpot{r: r, dir: t.TempDir(), owner: testOwner, api: &awsClient{
		region: r.Region, creds: &awsCredResolver{fixed: &awsCreds{AccessKeyID: "AK", SecretAccessKey: "s"}},
		http: srv.Client(), endpoints: map[string]string{"ec2": srv.URL},
	}}, &n
}

// A burst of throttling longer than one Query call's retry budget doesn't
// end the wait: the VM comes up within its boot deadline and is used.
func TestWaitRunningRidesOutThrottleBurst(t *testing.T) {
	a, n := newBootEnv(t, time.Minute, func(n int) (int, string) {
		if n <= 12 { // more than two Query calls' worth of attempts
			return http.StatusServiceUnavailable, bootThrottled
		}
		return http.StatusOK, bootInstance("running", "203.0.113.7")
	})
	in, err := a.WaitRunning(context.Background(), "i-boot", nil)
	if err != nil {
		t.Fatalf("err = %v after %d reads", err, n.Load())
	}
	if in.ID != "i-boot" || in.Target.Host != "203.0.113.7" || in.State != StateRunning {
		t.Fatalf("instance = %+v", in)
	}
}

// A network failure on the status read is ridden out the same way.
func TestWaitRunningRidesOutNetworkErrors(t *testing.T) {
	a, n := newBootEnv(t, time.Minute, func(n int) (int, string) {
		if n <= 6 {
			return http.StatusBadGateway, "<html>bad gateway</html>"
		}
		return http.StatusOK, bootInstance("running", "203.0.113.8")
	})
	if in, err := a.WaitRunning(context.Background(), "i-boot", nil); err != nil || in.Target.Host != "203.0.113.8" {
		t.Fatalf("instance = %+v, err = %v after %d reads", in, err, n.Load())
	}
}

// A terminal state read after an outage still ends the wait at once:
// the VM is gone, and waiting out the deadline would gain nothing.
func TestWaitRunningTerminalStateAfterOutage(t *testing.T) {
	a, n := newBootEnv(t, time.Minute, func(n int) (int, string) {
		if n <= 7 {
			return http.StatusServiceUnavailable, bootThrottled
		}
		return http.StatusOK, bootInstance("shutting-down", "")
	})
	start := time.Now()
	_, err := a.WaitRunning(context.Background(), "i-boot", nil)
	if err == nil || !strings.Contains(err.Error(), `"shutting-down"`) {
		t.Fatalf("err = %v after %d reads", err, n.Load())
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
}

// An outage that lasts through the boot deadline ends the wait at the
// deadline, naming the read failure that left the VM's state unknown.
func TestWaitRunningOutageThroughDeadline(t *testing.T) {
	a, n := newBootEnv(t, 300*time.Millisecond, func(int) (int, string) {
		return http.StatusServiceUnavailable, bootThrottled
	})
	start := time.Now()
	_, err := a.WaitRunning(context.Background(), "i-boot", nil)
	if err == nil || awsCode(err) != "RequestLimitExceeded" || !strings.Contains(err.Error(), "i-boot") {
		t.Fatalf("err = %v after %d reads", err, n.Load())
	}
	if el := time.Since(start); el < 300*time.Millisecond || el > 5*time.Second {
		t.Fatalf("gave up after %s, want at the 300ms deadline", el)
	}
	if n.Load() <= 5 {
		t.Fatalf("only %d reads: gave up with one Query call's retries", n.Load())
	}
}

// Cancelling the caller ends the wait with the context's error, not the
// read failure, so the caller knows nothing was learnt about the VM.
func TestWaitRunningCancelledDuringOutage(t *testing.T) {
	a, _ := newBootEnv(t, time.Minute, func(int) (int, string) {
		return http.StatusServiceUnavailable, bootThrottled
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := a.WaitRunning(ctx, "i-boot", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's", err)
	}
}

// A read error that retrying can't fix still ends the wait at once.
func TestWaitRunningFatalReadError(t *testing.T) {
	a, n := newBootEnv(t, time.Minute, func(int) (int, string) {
		return http.StatusForbidden, bootDenied
	})
	if _, err := a.WaitRunning(context.Background(), "i-boot", nil); awsCode(err) != "UnauthorizedOperation" || n.Load() != 1 {
		t.Fatalf("err = %v after %d reads", err, n.Load())
	}
}
