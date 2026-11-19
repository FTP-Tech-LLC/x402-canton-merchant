package flushwalk_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/flushwalk"
)

var errBroken = errors.New("broken pipe")

// conn is the innermost writer: it reports flush errors like net/http's response.
type conn struct {
	*httptest.ResponseRecorder
	err     error
	flushed int
}

func (c *conn) FlushError() error { c.flushed++; return c.err }

// buffering has Flush (which drops errors) and Unwrap, like gzip with a pass-through.
type buffering struct {
	http.ResponseWriter
	flushed int
}

func (b *buffering) Flush()                      { b.flushed++ }
func (b *buffering) Unwrap() http.ResponseWriter { return b.ResponseWriter }

// panicky is a wrapper whose Flush asserts a Flusher below it (gin, BodyDump).
type panicky struct{ http.ResponseWriter }

func (p panicky) Flush()                      { panic("writer cannot flush") }
func (p panicky) Unwrap() http.ResponseWriter { return p.ResponseWriter }

type opaque struct{ http.ResponseWriter } // neither Flush nor Unwrap

func TestWalkFindsTheConnectionErrorBehindBufferingWrappers(t *testing.T) {
	inner := &conn{ResponseRecorder: httptest.NewRecorder(), err: errBroken}
	outer := &buffering{ResponseWriter: panicky{&buffering{ResponseWriter: inner}}}
	seen, err := flushwalk.Flush(outer)
	assert.True(t, seen)
	assert.ErrorIs(t, err, errBroken)
	assert.Equal(t, 1, outer.flushed, "wrappers are flushed so they push their bytes down")
}

func TestWalkSeesAHealthyConnection(t *testing.T) {
	inner := &conn{ResponseRecorder: httptest.NewRecorder()}
	seen, err := flushwalk.Flush(&buffering{ResponseWriter: inner})
	assert.True(t, seen)
	assert.NoError(t, err)
}

func TestWalkIsBlindBehindAWrapperWithoutUnwrap(t *testing.T) {
	inner := &conn{ResponseRecorder: httptest.NewRecorder(), err: errBroken}
	seen, err := flushwalk.Flush(opaque{inner})
	assert.False(t, seen, "the connection cannot be reached")
	assert.NoError(t, err)
	seen, _ = flushwalk.Flush(httptest.NewRecorder())
	assert.False(t, seen, "a writer with no FlushError cannot report")
}

func TestWalkTreatsNotSupportedAsBlind(t *testing.T) {
	seen, err := flushwalk.Flush(notSupported{httptest.NewRecorder()})
	assert.False(t, seen)
	assert.NoError(t, err)
}

type notSupported struct{ http.ResponseWriter }

func (notSupported) FlushError() error { return http.ErrNotSupported }
