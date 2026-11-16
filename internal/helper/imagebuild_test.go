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
