package helper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// BuildKit's output for a RUN step that exits 2.
const buildkitRunFailed = `#0 building with "default" instance using docker driver

#1 [internal] load build definition from Dockerfile
#1 transferring dockerfile: 92B done
#1 DONE 0.0s

#5 [2/2] RUN make deps
#5 0.214 make: *** No rule to make target 'deps'.  Stop.
#5 ERROR: process "/bin/sh -c make deps" did not complete successfully: exit code: 2
------
 > [2/2] RUN make deps:
0.214 make: *** No rule to make target 'deps'.  Stop.
------
Dockerfile:2
--------------------
   1 |     FROM debian:bookworm
   2 | >>> RUN make deps
--------------------
ERROR: failed to solve: process "/bin/sh -c make deps" did not complete successfully: exit code: 2
`

// classify feeds out to a buildLog as stderr, in small chunks that split
// lines, and returns the error for a build that exited with pr.
func classify(out string, pr procResult) error {
	var b buildLog
	for i := 0; i < len(out); i += 7 {
		b.write(1, []byte(out[i:min(i+7, len(out))]))
	}
	return b.failure("ci/Dockerfile", pr, fmt.Errorf("exit %d", pr.code))
}

func codeOf(t *testing.T, err error) *protoError {
	t.Helper()
	var pe *protoError
	if !errors.As(err, &pe) {
		t.Fatalf("not a protocol error: %v", err)
	}
	return pe
}

// #193: a build that fails because of the Dockerfile is the project's
// failure (IMAGE_BUILD_FAILED); one that fails around it stays spotexec's.
func TestBuildFailureClassification(t *testing.T) {
	exit1 := procResult{code: 1}
	for _, tc := range []struct {
		name string
		out  string
		pr   procResult
		want string // the code; for IMAGE_BUILD_FAILED, what the message says
	}{
		{"BuildKit RUN step", buildkitRunFailed, exit1, "a RUN step exited 2"},
		{"legacy RUN step", "Step 2/2 : RUN make deps\n ---> Running in 0a1b\nmake: *** No rule to make target 'deps'.\nThe command '/bin/sh -c make deps' returned a non-zero code: 2\n", procResult{code: 2}, "a RUN step exited 2"},
		{"legacy RUN step exiting 125", "The command '/bin/sh -c exit 125' returned a non-zero code: 125\n", procResult{code: 125}, "a RUN step exited 125"},
		{"BuildKit syntax error", "ERROR: failed to solve: dockerfile parse error on line 2: unknown instruction: RUNN\n", exit1, "doesn't parse"},
		{"legacy syntax error", "Error response from daemon: Dockerfile parse error line 2: unknown instruction: RUNN\n", exit1, "doesn't parse"},
		{"BuildKit missing COPY source", "ERROR: failed to solve: failed to compute cache key: failed to calculate checksum of ref 8f2c::xyz: \"/missing.txt\": not found\n", exit1, "isn't in the build context"},
		{"legacy missing COPY source", "COPY failed: file not found in build context or excluded by .dockerignore: stat missing.txt: file does not exist\n", exit1, "isn't in the build context"},
		{"older BuildKit RUN step", "#5 ERROR: executor failed running [/bin/sh -c make deps]: exit code: 2\nfailed to solve with frontend dockerfile.v0: failed to build LLB: executor failed running [/bin/sh -c make deps]: exit code: 2\n", exit1, "a RUN step exited 2"},
		{"older BuildKit missing COPY source", "error: failed to solve: failed to compute cache key: \"/missing.txt\" not found: not found\n", exit1, "isn't in the build context"},
		{"legacy missing ADD source", "ADD failed: file not found in build context or excluded by .dockerignore: stat missing.txt: file does not exist\n", exit1, "isn't in the build context"},
		{"older legacy missing COPY source", "COPY failed: stat /var/lib/docker/tmp/docker-builder123/missing.txt: no such file or directory\n", exit1, "isn't in the build context"},
		{"RUN step failing to connect", "#5 0.5 curl: (7) Failed to connect to example.com port 443 after 3 ms: Couldn't connect to server\n#5 ERROR: process \"/bin/sh -c curl -fsSL https://example.com/x\" did not complete successfully: exit code: 7\n", exit1, protocol.CodeImageFailed},
		{"RUN step getting a 503", "#5 0.5 curl: (22) The requested URL returned error: 503\n#5 ERROR: process \"/bin/sh -c curl -f https://example.com/x\" did not complete successfully: exit code: 22\n", exit1, protocol.CodeImageFailed},
		{"RUN step with npm offline", "#5 3.1 npm ERR! code EAI_AGAIN\n#5 ERROR: process \"/bin/sh -c npm ci\" did not complete successfully: exit code: 1\n", exit1, protocol.CodeImageFailed},
		{"a step printing a diagnostic", "#5 0.3 test: dockerfile parse error on line 1 (expected)\n#5 0.3 x returned a non-zero code: 1\n#5 DONE 0.4s\nERROR: failed to solve: something new\n", exit1, protocol.CodeImageFailed},
		{"the summary repeating a step's line", "------\n > [2/2] RUN ./check:\n0.214 x did not complete successfully: exit code: 3\n------\nERROR: failed to solve: something new\n", exit1, protocol.CodeImageFailed},
		{"apt failing on DNS", "#5 1.0 Err:1 http://deb.debian.org/debian bookworm InRelease\n#5 1.0   Temporary failure resolving 'deb.debian.org'\n#5 ERROR: process \"/bin/sh -c apt-get update && apt-get install -y foo\" did not complete successfully: exit code: 100\n", exit1, protocol.CodeImageFailed},
		{"a RUN command quoting a network error", "#5 [2/2] RUN grep -q ECONNRESET /app/test.log\n#5 ERROR: process \"/bin/sh -c grep -q ECONNRESET /app/test.log\" did not complete successfully: exit code: 1\n------\n > [2/2] RUN grep -q ECONNRESET /app/test.log:\n------\nDockerfile:2\n   2 | >>> RUN grep -q ECONNRESET /app/test.log\nERROR: failed to solve: process \"/bin/sh -c grep -q ECONNRESET /app/test.log\" did not complete successfully: exit code: 1\n", exit1, "a RUN step exited 1"},
		{"a legacy RUN command quoting a network error", "Step 2/2 : RUN grep -q ECONNRESET log\nThe command '/bin/sh -c grep -q ECONNRESET log' returned a non-zero code: 1\n", exit1, "a RUN step exited 1"},
		{"a step heading quoting a diagnostic", "#5 [2/2] RUN ./tests --grep \"dockerfile parse error\"\n#5 ERROR: process \"/bin/sh -c ./tests --grep \\\"dockerfile parse error\\\"\" did not complete successfully: exit code: 137\n", exit1, protocol.CodeImageFailed},
		{"a step's progress rewriting its line", "#5 0.214 checking\rdockerfile parse error (expected)\n#5 ERROR: process \"/bin/sh -c ./check\" did not complete successfully: exit code: 137\n", exit1, protocol.CodeImageFailed},
		{"daemon down", "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n", exit1, protocol.CodeImageFailed},
		{"base image missing", "ERROR: failed to solve: debian:nope: failed to resolve source metadata for docker.io/library/debian:nope: docker.io/library/debian:nope: not found\n", exit1, protocol.CodeImageFailed},
		{"RUN step failing on DNS", "#5 1.2 Temporary failure in name resolution\n#5 ERROR: process \"/bin/sh -c apt-get update\" did not complete successfully: exit code: 100\n", exit1, protocol.CodeImageFailed},
		{"RUN step killed", "#5 ERROR: process \"/bin/sh -c npm ci\" did not complete successfully: exit code: 137\n", exit1, protocol.CodeImageFailed},
		{"disk full", buildkitRunFailed + "write /var/lib/docker/tmp: no space left on device\n", exit1, protocol.CodeImageFailed},
		{"timed out", buildkitRunFailed, procResult{code: 124, timedOut: true}, protocol.CodeImageFailed},
		{"cancelled", buildkitRunFailed, procResult{code: 143, canceled: true}, protocol.CodeImageFailed},
		{"killed by a signal", buildkitRunFailed, procResult{code: 137, signal: "KILL"}, protocol.CodeImageFailed},
		{"no output", "", exit1, protocol.CodeImageFailed},
		{"unrecognised output", "ERROR: something new went wrong\n", exit1, protocol.CodeImageFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pe := codeOf(t, classify(tc.out, tc.pr))
			if tc.want == protocol.CodeImageFailed {
				if pe.code != protocol.CodeImageFailed || pe.detail != "" {
					t.Fatalf("got %s (%q), want IMAGE_FAILED", pe.code, pe.msg)
				}
				return
			}
			if pe.code != protocol.CodeImageBuildFailed || !strings.Contains(pe.msg, "ci/Dockerfile") || !strings.Contains(pe.msg, tc.want) {
				t.Fatalf("got %s: %q", pe.code, pe.msg)
			}
			if !strings.Contains(pe.detail, strings.TrimSpace(tc.out[strings.LastIndex(strings.TrimSpace(tc.out), "\n")+1:])) {
				t.Fatalf("detail lacks the output's last line: %q", pe.detail)
			}
		})
	}
}

// An endless line still counts for a failure around the build, though only
// its end is kept: its start is looked at before it's dropped.
func TestBuildFailureLongLine(t *testing.T) {
	long := "#5 0.1 Temporary failure in name resolution " + strings.Repeat("x", 3*buildLineMax)
	for name, out := range map[string]string{
		"unfinished, then ended": long + "\n" + buildkitRunFailed,
		"in one piece":           buildkitRunFailed + long + "\n",
	} {
		var b buildLog
		if name == "in one piece" {
			b.write(1, []byte(out))
		} else {
			for i := 0; i < len(out); i += 1000 {
				b.write(1, []byte(out[i:min(i+1000, len(out))]))
			}
		}
		pe := codeOf(t, b.failure("Dockerfile", procResult{code: 1}, errors.New("exit 1")))
		if pe.code != protocol.CodeImageFailed {
			t.Errorf("%s: got %s", name, pe.code)
		}
	}
	// A long line whose start was dropped can't show the Dockerfile at
	// fault: that start would say whether a step printed it.
	var b buildLog
	out := "#5 0.1 " + strings.Repeat("y", 2*buildLineMax) + ` did not complete successfully: exit code: 2` + "\n"
	for i := 0; i < len(out); i += 1000 {
		b.write(1, []byte(out[i:min(i+1000, len(out))]))
	}
	if pe := codeOf(t, b.failure("Dockerfile", procResult{code: 1}, errors.New("exit 1"))); pe.code != protocol.CodeImageFailed {
		t.Errorf("long step line: got %s", pe.code)
	}
	if len(b.tail) != 1 || len(b.tail[0]) != buildLineMax {
		t.Errorf("tail holds %d lines, the last of %d bytes", len(b.tail), len(b.tail[len(b.tail)-1]))
	}
}

// The detail is the build's last lines, however long the build: the
// evidence seen early still counts once it has scrolled out of the tail.
func TestBuildFailureTail(t *testing.T) {
	var out strings.Builder
	out.WriteString("#5 ERROR: process \"/bin/sh -c false\" did not complete successfully: exit code: 1\n")
	for i := range 100 {
		fmt.Fprintf(&out, "line %d\r\n", i)
	}
	out.WriteString("progress 10%\rprogress 100%") // unterminated, rewritten
	pe := codeOf(t, classify(out.String(), procResult{code: 1}))
	if pe.code != protocol.CodeImageBuildFailed {
		t.Fatalf("got %s", pe.code)
	}
	lines := strings.Split(pe.detail, "\n")
	if len(lines) != buildTailLines || lines[0] != "line 61" || lines[len(lines)-1] != "progress 100%" {
		t.Fatalf("tail %q", lines)
	}
}

// The build's output still streams to the client while it is classified,
// and the error carries its end.
func TestBuildStreamsAndClassifies(t *testing.T) {
	s := testSession(t, t.TempDir())
	if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 1}); err != nil {
		t.Fatal(err)
	}
	out := &syncBuf{}
	s.c = protocol.NewConn(strings.NewReader(""), out)
	script := filepath.Join(t.TempDir(), "build.sh")
	if err := os.WriteFile(script, []byte("cat >&2 <<'EOF'\n"+buildkitRunFailed+"EOF\necho step output\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	up := func() bool { return true }
	pe := codeOf(t, s.build(ctx, "sh "+shq(script), "Dockerfile", up))
	if pe.code != protocol.CodeImageBuildFailed || !strings.Contains(pe.detail, "ERROR: failed to solve") || !pe.buildFailed {
		t.Fatalf("got %s: %q, detail %q, build failed %v", pe.code, pe.msg, pe.detail, pe.buildFailed)
	}
	var stdout, stderr string
	in := protocol.NewConn(bytes.NewReader(out.b.Bytes()), io.Discard)
	for {
		f, err := in.ReadFrame()
		if err != nil {
			break
		}
		switch f.Kind {
		case protocol.KindStdout:
			stdout += string(f.Data)
		case protocol.KindStderr:
			stderr += string(f.Data)
		}
	}
	if stdout != "step output\n" || stderr != buildkitRunFailed {
		t.Fatalf("streamed stdout %q, stderr %q", stdout, stderr)
	}

	// A build that succeeds is no error.
	if err := s.build(ctx, "true", "Dockerfile", up); err != nil {
		t.Fatal(err)
	}

	// Either code is marked as the build failing (#194) only while the
	// daemon still answers, and only when docker build itself failed.
	if pe := codeOf(t, s.build(ctx, "sh "+shq(script), "Dockerfile", func() bool { return false })); pe.code != protocol.CodeImageBuildFailed || pe.buildFailed {
		t.Fatalf("daemon gone: got %s, build failed %v", pe.code, pe.buildFailed)
	}
	if pe := codeOf(t, s.build(ctx, "exit 1", "Dockerfile", up)); pe.code != protocol.CodeImageFailed || !pe.buildFailed {
		t.Fatalf("plain exit 1: got %s, build failed %v", pe.code, pe.buildFailed)
	}
	if pe := codeOf(t, s.build(ctx, "exit 127", "Dockerfile", up)); pe.code != protocol.CodeImageFailed || pe.buildFailed {
		t.Fatalf("exit 127: got %s, build failed %v", pe.code, pe.buildFailed)
	}
}
