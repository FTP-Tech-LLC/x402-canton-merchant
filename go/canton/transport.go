package canton

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// answerRecorder wraps the HTTP transport of the facilitator client. The SDK
// client turns most failures into plain errors that carry no status or headers;
// this records the status and Retry-After of the last answer into the request
// state, so the client wrapper can tell a refusal from an unknown outcome.
type answerRecorder struct {
	base http.RoundTripper
}

func (t answerRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	if state := stateFrom(req.Context()); state != nil {
		state.recordAnswer(resp.StatusCode, parseRetryAfter(resp.Header.Get("Retry-After")))
	}
	return resp, nil
}

// parseRetryAfter reads the delay-seconds form. The HTTP-date form is ignored:
// the facilitator sends seconds.
func parseRetryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}
