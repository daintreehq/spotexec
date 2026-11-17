package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// bootReply is the fake EC2 endpoint's answer to one status read: an HTTP
// status and body, a transport error, or a read that hangs until its
// context ends (and then, with late, answers anyway).
type bootReply struct {
	status int
	body   string
	err    error
	hang   bool
	late   bool
}

func bootErr(status int, code string) bootReply {
	return bootReply{status: status, body: fmt.Sprintf(`<Response><Errors><Error><Code>%s</Code><Message>%s.</Message></Error></Errors></Response>`, code, code)}
}

func bootInstance(state, ip string) bootReply {
	return bootReply{status: http.StatusOK, body: fmt.Sprintf(`<DescribeInstancesResponse><reservationSet><item><instancesSet><item><instanceId>i-boot</instanceId><instanceType>c7a.xlarge</instanceType><instanceState><name>%s</name></instanceState><ipAddress>%s</ipAddress></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`, state, ip)}
}

var (
	throttled = bootErr(http.StatusServiceUnavailable, "RequestLimitExceeded")
	up        = bootInstance("running", "203.0.113.7")
)

type bootEnv struct {
	a     *AWSSpot
	reads atomic.Int32 // DescribeInstances calls answered by the script
	mu    sync.Mutex
	other []string // any other action called
}

func (e *bootEnv) others() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.other...)
}

// newBootEnv answers the nth DescribeInstances call with script(n), in
// memory, so a test run in a synctest bubble uses the real poll interval,
// Query backoff and boot timeout on a fake clock.
func newBootEnv(t *testing.T, bootTimeout time.Duration, script func(n int) bootReply) *bootEnv {
	t.Helper()
	e := &bootEnv{}
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		v, _ := url.ParseQuery(string(b))
		if a := v.Get("Action"); a != "DescribeInstances" {
			e.mu.Lock()
			e.other = append(e.other, a)
			e.mu.Unlock()
			return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(bootErr(400, "UnexpectedAction").body)), Request: r}, nil
		}
		rep := script(int(e.reads.Add(1)))
		switch {
		case rep.hang:
			<-r.Context().Done()
			if !rep.late {
				return nil, r.Context().Err()
			}
		case rep.err != nil:
			return nil, rep.err
		}
		return &http.Response{StatusCode: rep.status, Body: io.NopCloser(strings.NewReader(rep.body)), Request: r}, nil
	})
	r := &config.Runner{Name: "aws", Type: "aws-spot", Region: "us-east-2", User: "ubuntu"}
	r.BootTimeout.Duration = bootTimeout
	e.a = &AWSSpot{r: r, dir: t.TempDir(), owner: testOwner, api: &awsClient{
		region: r.Region, creds: &awsCredResolver{fixed: &awsCreds{AccessKeyID: "AK", SecretAccessKey: "s"}},
		http: &http.Client{Transport: rt}, endpoints: map[string]string{"ec2": "https://ec2.test/"},
	}}
	return e
}

// A burst of throttling longer than one Query call's retries doesn't end
// the wait: the VM comes up within its boot deadline and is used. EC2
// throttles with a 503; a 400 carrying the same code is retried alike.
func TestWaitRunningRidesOutThrottleBurst(t *testing.T) {
	for _, c := range []struct {
		status, reads int
	}{{503, 5}, {400, 5}, {503, 12}} {
		t.Run(fmt.Sprintf("%dx%d", c.status, c.reads), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newBootEnv(t, time.Minute, func(n int) bootReply {
					if n <= c.reads {
						return bootErr(c.status, "RequestLimitExceeded")
					}
					return up
				})
				start := time.Now()
				in, err := e.a.WaitRunning(context.Background(), "i-boot", nil)
				if err != nil {
					t.Fatalf("err = %v after %d reads", err, e.reads.Load())
				}
				if in.ID != "i-boot" || in.Target.Host != "203.0.113.7" || in.State != StateRunning {
					t.Fatalf("instance = %+v", in)
				}
				if el := time.Since(start); el >= time.Minute {
					t.Fatalf("took %s", el)
				}
			})
		})
	}
}

// Transport failures (a reset connection) are ridden out the same way.
func TestWaitRunningRidesOutTransportErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newBootEnv(t, time.Minute, func(n int) bootReply {
			if n <= 6 {
				return bootReply{err: errors.New("connection reset by peer")}
			}
			return up
		})
		if in, err := e.a.WaitRunning(context.Background(), "i-boot", nil); err != nil || in.Target.Host != "203.0.113.7" {
			t.Fatalf("instance = %+v, err = %v after %d reads", in, err, e.reads.Load())
		}
	})
}

// A terminal state read after an outage still ends the wait at once: the
// VM is gone, and waiting out the deadline would gain nothing.
func TestWaitRunningTerminalStateAfterOutage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newBootEnv(t, time.Minute, func(n int) bootReply {
			if n <= 7 {
				return throttled
			}
			return bootInstance("shutting-down", "")
		})
		_, err := e.a.WaitRunning(context.Background(), "i-boot", nil)
		if err == nil || !strings.Contains(err.Error(), `"shutting-down"`) || e.reads.Load() != 8 {
			t.Fatalf("err = %v after %d reads", err, e.reads.Load())
		}
	})
}

// A no-IP observation from before an outage doesn't count the outage as
// time without an IP: after it, the VM gets the full 45 seconds again.
func TestWaitRunningNoIPTimerSkipsOutage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newBootEnv(t, 10*time.Minute, func(n int) bootReply {
			switch {
			case n == 1:
				return bootInstance("running", "")
			case n <= 31: // six exhausted Query calls: well over 45s
				return throttled
			case n == 32:
				return bootInstance("running", "")
			}
			return up
		})
		in, err := e.a.WaitRunning(context.Background(), "i-boot", nil)
		if err != nil || in.Target.Host != "203.0.113.7" {
			t.Fatalf("instance = %+v, err = %v after %d reads", in, err, e.reads.Load())
		}
		if o := e.others(); len(o) > 0 {
			t.Fatalf("also called %v", o)
		}
	})
}

// An outage that lasts through the boot deadline ends the wait at the
// deadline, naming the read failure that left the VM's state unknown.
func TestWaitRunningOutageThroughDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newBootEnv(t, time.Minute, func(int) bootReply { return throttled })
		start := time.Now()
		_, err := e.a.WaitRunning(context.Background(), "i-boot", nil)
		if awsCode(err) != "RequestLimitExceeded" || !strings.Contains(err.Error(), "i-boot") {
			t.Fatalf("err = %v after %d reads", err, e.reads.Load())
		}
		if el := time.Since(start); el != time.Minute {
			t.Fatalf("gave up after %s, want at the one-minute deadline", el)
		}
		if e.reads.Load() <= 5 {
			t.Fatalf("only %d reads: gave up with one Query call's retries", e.reads.Load())
		}
	})
}

// When the boot deadline cuts a read short, the error names the latest
// failure, not the first, and not the deadline itself.
func TestWaitRunningDeadlineNamesLatestFailure(t *testing.T) {
	errB, errC := errors.New("connection reset B"), errors.New("connection reset C")
	for _, c := range []struct {
		name    string
		timeout time.Duration
		script  func(n int) bootReply
		check   func(error) bool
	}{
		{"first read's retries cut short", 3300 * time.Millisecond, func(int) bootReply {
			return bootErr(400, "RequestLimitExceeded") // the second attempt would start after the deadline
		}, func(err error) bool { return awsCode(err) == "RequestLimitExceeded" }},
		{"latest of several", 30 * time.Second, func(n int) bootReply {
			switch {
			case n <= 5:
				return throttled
			case n == 6:
				return bootReply{err: errB}
			case n == 7:
				return bootReply{err: errC}
			}
			return bootReply{hang: true}
		}, func(err error) bool { return errors.Is(err, errC) && !errors.Is(err, errB) && awsCode(err) == "" }},
		{"not found after an outage", 30 * time.Second, func(n int) bootReply {
			switch {
			case n <= 5:
				return throttled
			case n == 6:
				return bootErr(400, "InvalidInstanceID.NotFound")
			}
			return bootReply{hang: true}
		}, func(err error) bool { return awsCode(err) == "RequestLimitExceeded" }},
		{"first read hangs", 10 * time.Second, func(int) bootReply {
			return bootReply{hang: true}
		}, func(err error) bool { return strings.Contains(err.Error(), "did not answer") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newBootEnv(t, c.timeout, c.script)
				start := time.Now()
				_, err := e.a.WaitRunning(context.Background(), "i-boot", nil)
				if err == nil || !c.check(err) || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("err = %v after %d reads", err, e.reads.Load())
				}
				if el := time.Since(start); el != c.timeout {
					t.Fatalf("gave up after %s, want at the %s deadline", el, c.timeout)
				}
			})
		})
	}
}

// The caller ending the wait mid-outage, by cancelling or by its own
// earlier deadline, gets the context's error, not the read failure, so it
// knows nothing was learnt about the VM.
func TestWaitRunningCallerEndsDuringOutage(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e := newBootEnv(t, time.Minute, func(n int) bootReply {
				if n == 7 { // after one exhausted Query call
					cancel()
				}
				return throttled
			})
			if _, err := e.a.WaitRunning(ctx, "i-boot", nil); !errors.Is(err, context.Canceled) || awsCode(err) != "" {
				t.Fatalf("err = %v after %d reads", err, e.reads.Load())
			}
		})
	})
	t.Run("caller deadline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			e := newBootEnv(t, time.Minute, func(int) bootReply { return throttled })
			if _, err := e.a.WaitRunning(ctx, "i-boot", nil); !errors.Is(err, context.DeadlineExceeded) || awsCode(err) != "" || e.reads.Load() <= 5 {
				t.Fatalf("err = %v after %d reads", err, e.reads.Load())
			}
		})
	})
}

// A read error that retrying can't fix ends the wait at once.
func TestWaitRunningFatalReadError(t *testing.T) {
	for _, c := range []struct {
		status int
		code   string
	}{{403, "UnauthorizedOperation"}, {400, "UnsupportedOperation"}} {
		t.Run(c.code, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newBootEnv(t, time.Minute, func(n int) bootReply {
					if n == 1 {
						return bootErr(c.status, c.code)
					}
					return up // a retry would succeed, so one fails the test
				})
				if _, err := e.a.WaitRunning(context.Background(), "i-boot", nil); awsCode(err) != c.code || e.reads.Load() != 1 {
					t.Fatalf("err = %v after %d reads", err, e.reads.Load())
				}
			})
		})
	}
}

// Credentials that can't be had or are rejected aren't an outage: the wait
// ends with the credential error itself.
func TestWaitRunningCredentialFailure(t *testing.T) {
	t.Run("none to sign with", func(t *testing.T) {
		t.Setenv("AWS_ACCESS_KEY_ID", "")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "")
		synctest.Test(t, func(t *testing.T) {
			e := newBootEnv(t, time.Minute, func(int) bootReply { return up })
			e.a.api.creds = &awsCredResolver{envOnly: true}
			start := time.Now()
			_, err := e.a.WaitRunning(context.Background(), "i-boot", nil)
			ce, ok := err.(*CredentialError) // itself, not wrapped in a boot timeout
			if !ok || ce.Err != errNoPassedSession || e.reads.Load() != 0 || time.Since(start) > 5*time.Second {
				t.Fatalf("err = %T %v after %d reads", err, err, e.reads.Load())
			}
		})
	})
	t.Run("rejected", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newBootEnv(t, time.Minute, func(int) bootReply { return bootErr(401, "AuthFailure") })
			_, err := e.a.WaitRunning(context.Background(), "i-boot", nil)
			if _, ok := err.(*CredentialError); !ok || e.reads.Load() > 2 {
				t.Fatalf("err = %T %v after %d reads", err, err, e.reads.Load())
			}
		})
	})
	t.Run("rejected as the boot deadline passes", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newBootEnv(t, 10*time.Second, func(n int) bootReply {
				r := bootErr(401, "AuthFailure")
				r.hang, r.late = n == 2, true // the refreshed attempt answers only after the deadline
				return r
			})
			_, err := e.a.WaitRunning(context.Background(), "i-boot", nil)
			if _, ok := err.(*CredentialError); !ok || e.reads.Load() != 2 {
				t.Fatalf("err = %T %v after %d reads", err, err, e.reads.Load())
			}
		})
	})
}
