package helper

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// buildTailLines is how much of a failed build's output its error carries:
// enough for BuildKit's closing summary of the failing step.
const buildTailLines = 40

// buildLineMax caps a line held for classification or the tail.
const buildLineMax = 4 << 10

// Evidence that the project's Dockerfile itself is broken (#193). Docker's
// build output is not a typed API, so only these well-known diagnostics
// count; anything else stays a failure of spotexec's own (exit 125).
var (
	// BuildKit: a RUN step that exited non-zero.
	buildRunExitRe = regexp.MustCompile(`did not complete successfully: exit code: (\d+)`)
	// The legacy builder's form of the same.
	legacyRunExitRe = regexp.MustCompile(`returned a non-zero code: (\d+)`)
	// A Dockerfile that doesn't parse ("dockerfile parse error on line 3:
	// unknown instruction: RUNN"; the legacy builder's "Dockerfile parse
	// error line 3: …").
	parseErrorRe = regexp.MustCompile(`(?i)dockerfile parse error`)
	// COPY or ADD of a file that isn't in the build context (BuildKit, then
	// the legacy builder).
	missingSourceRe = regexp.MustCompile(`failed to calculate checksum of ref [^ ]+ "[^"]*": not found|COPY failed: file not found in build context`)
	// Evidence of Docker, the network or the machine failing, which wins
	// over the above: a RUN step that fails because DNS is down is not the
	// Dockerfile's fault.
	buildInfraRe = regexp.MustCompile(`(?i)cannot connect to the docker daemon|error during connect|no space left on device|temporary failure in name resolution|could not resolve host|no such host|network is unreachable|i/o timeout|tls handshake timeout|connection reset by peer|toomanyrequests|input/output error`)
)

// buildLog watches a docker build's output as it streams to the client:
// it keeps the last lines for the error, and what they say about why the
// build failed. Every line is classified as it arrives, so a long build
// can't push early evidence out of the tail.
type buildLog struct {
	mu      sync.Mutex
	partial [2][]byte // per stream, a line not yet ended
	tail    []string
	project string // why the Dockerfile is at fault, if it is
	infra   bool   // the output shows Docker, the network or the VM failing
}

// write takes a chunk of stream i's output (0 stdout, 1 stderr).
func (b *buildLog) write(i int, p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	buf := append(b.partial[i], p...)
	for {
		n := bytes.IndexByte(buf, '\n')
		if n < 0 {
			break
		}
		b.line(string(buf[:n]))
		buf = buf[n+1:]
	}
	if len(buf) > buildLineMax {
		// An endless line (a progress bar): keep its end only.
		buf = buf[len(buf)-buildLineMax:]
	}
	b.partial[i] = append([]byte(nil), buf...)
}

// line classifies one line and adds it to the tail.
func (b *buildLog) line(l string) {
	l = strings.TrimRight(l, "\r")
	if j := strings.LastIndexByte(l, '\r'); j >= 0 {
		l = l[j+1:]
	}
	if len(l) > buildLineMax {
		l = l[len(l)-buildLineMax:]
	}
	if buildInfraRe.MatchString(l) {
		b.infra = true
	}
	if b.project == "" {
		b.project = projectCause(l)
	}
	b.tail = append(b.tail, l)
	if len(b.tail) > buildTailLines {
		b.tail = b.tail[len(b.tail)-buildTailLines:]
	}
}

// projectCause says how line l shows the Dockerfile at fault, or "".
func projectCause(l string) string {
	for _, re := range []*regexp.Regexp{buildRunExitRe, legacyRunExitRe} {
		if m := re.FindStringSubmatch(l); m != nil {
			code, _ := strconv.Atoi(m[1])
			if code == 137 || code == 143 {
				// Killed (out of memory, or the VM stopping it): not
				// something the step did.
				return ""
			}
			return "a RUN step exited " + m[1]
		}
	}
	if parseErrorRe.MatchString(l) {
		return "the Dockerfile doesn't parse"
	}
	if missingSourceRe.MatchString(l) {
		return "a COPY or ADD source isn't in the build context"
	}
	return ""
}

// flush ends any unterminated lines.
func (b *buildLog) flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.partial {
		if len(b.partial[i]) > 0 {
			b.line(string(b.partial[i]))
			b.partial[i] = nil
		}
	}
}

// failure is the error for a build that exited with pr, naming dockerfile:
// IMAGE_BUILD_FAILED, with the output's tail as its detail, when the
// output shows the Dockerfile at fault and nothing else failing; otherwise
// IMAGE_FAILED, spotexec's own failure.
func (b *buildLog) failure(dockerfile string, pr procResult, err error) error {
	b.flush()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.project == "" || b.infra || pr.startErr != nil || pr.timedOut || pr.canceled || pr.spotIntr || pr.signal != "" {
		return perr(protocol.CodeImageFailed, "docker build %s: %v", dockerfile, err)
	}
	return &protoError{
		code:   protocol.CodeImageBuildFailed,
		msg:    "the project's Dockerfile " + dockerfile + " failed to build: " + b.project,
		detail: strings.Join(b.tail, "\n"),
	}
}
