package client

import (
	"errors"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/transport"
)

// imageErr is how a failed image step ends the run (err) and what runner,
// if any, it lost: a broken Dockerfile as itself, never replayed; anything
// else (a remembered failed build that isn't the Dockerfile's fault
// included) as spotexec's own failure.
func imageErr(lease *Lease, err error) (error, string) {
	var be *ImageBuildError
	if errors.As(err, &be) && be.Project() {
		return err, ""
	}
	return infra(err), lostRunner(lease, err)
}

// imageBuildError marks err as the project's Dockerfile failing to build
// when the runner said so, and returns it unchanged otherwise (an older
// runner's IMAGE_FAILED stays spotexec's own failure, and an error already
// an ImageBuildError stays as it is).
func imageBuildError(err error) error {
	var re *transport.RemoteError
	var be *ImageBuildError
	if errors.As(err, &re) && re.Code == protocol.CodeImageBuildFailed && !errors.As(err, &be) {
		return &ImageBuildError{Err: err, imageFailure: imageFailure{Code: re.Code, Message: re.Message, Tail: re.Detail}}
	}
	return err
}

// imageFailedEvent reports a failed image build: image_build_failed when
// the project's Dockerfile is at fault (#193), image_failed otherwise
// (#194). dockerfile is the Dockerfile built, took how long this run spent
// on the image.
func imageFailedEvent(o Options, be *ImageBuildError, dockerfile string, took time.Duration) {
	code, name := be.Code, "image_failed"
	if code == "" {
		code = protocol.CodeImageFailed
	}
	if be.Project() {
		name = "image_build_failed"
	}
	ev := map[string]any{"code": code, "message": be.Message, "tag": be.Tag, "cached": be.Cached, "duration_ms": took.Milliseconds()}
	if dockerfile != "" {
		ev["dockerfile"] = dockerfile
	}
	if be.Who != "" {
		ev["built_by"] = be.Who
	}
	if !be.At.IsZero() {
		ev["failed_at"] = be.At.UTC().Format(time.RFC3339)
	}
	if !be.Until.IsZero() {
		ev["retry_after"] = be.Until.UTC().Format(time.RFC3339)
	}
	if be.Tail != "" {
		ev["output_tail"], ev["output_truncated"] = be.Tail, be.Truncated
	}
	if be.Log != "" {
		ev["log"] = be.Log
	}
	o.Rep.Event(name, ev)
}
