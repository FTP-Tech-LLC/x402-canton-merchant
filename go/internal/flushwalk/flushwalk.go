// Package flushwalk flushes a response through a chain of writer wrappers and
// returns the connection's own flush error.
//
// net/http's http.ResponseController.Flush stops at the first writer in the
// chain that has a Flush method, and those methods (gzip, body dumps, status
// recorders, gin's and echo's writers) return nothing: a failed write to the
// client is lost, and a paid answer that never arrived looks delivered. The
// walk calls Flush on every wrapper, so buffering ones push their bytes down,
// then follows Unwrap to the writer that can report the error (net/http's own
// response has FlushError, whose bufio error is sticky).
package flushwalk

import (
	"errors"
	"net/http"
)

// maxDepth bounds the walk against a wrapper that unwraps to itself.
const maxDepth = 64

// Flush flushes w through its Unwrap chain. seen is false when no writer in the
// chain can report a flush error (a wrapper without Unwrap, a writer without
// Flush): the caller then cannot tell whether the answer reached the client.
// When seen is true, err is the connection's flush error, or nil.
func Flush(w http.ResponseWriter) (seen bool, err error) {
	for depth := 0; w != nil && depth < maxDepth; depth++ {
		if reporter, ok := w.(interface{ FlushError() error }); ok {
			flushErr := reporter.FlushError()
			if errors.Is(flushErr, http.ErrNotSupported) {
				return false, nil
			}
			return true, flushErr
		}
		if flusher, ok := w.(http.Flusher); ok {
			flushQuietly(flusher)
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		w = unwrapper.Unwrap()
	}
	return false, nil
}

// flushQuietly calls Flush and swallows the panic of a wrapper (gin's, echo's,
// BodyDump) whose Flush asserts that the writer under it can flush.
func flushQuietly(f http.Flusher) {
	defer func() { _ = recover() }()
	f.Flush()
}
