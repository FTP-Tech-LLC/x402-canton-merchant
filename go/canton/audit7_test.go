package canton

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A wrapper in front of the gate that has Flush() and Unwrap(), like a gzip, a
// logging or a status-recorder middleware. Its Flush() returns nothing, so
// http.ResponseController stops there and the connection's error is lost.
type flushingWrapper struct{ http.ResponseWriter }

func (w flushingWrapper) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w flushingWrapper) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// opaqueWrapper has neither Unwrap nor FlushError: the connection is out of reach.
type opaqueWrapper struct{ http.ResponseWriter }

func (w opaqueWrapper) Flush() {}

func TestFailedWriteBehindAFlushingWrapperGivesTheClaimBack(t *testing.T) {
	f := startMock(t)
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits))
	sig := signatureFor(t, h, testPreparedTx)

	broken := newDeadlineRecorder()
	broken.flushErr = errors.New("i/o timeout") // the connection breaks at the final flush
	serveWith(h, flushingWrapper{broken}, sig)

	f.settle = func(_ int32, w http.ResponseWriter) { settleAlreadySettled(w, testUpdateID) }
	retry := do(h, "GET", "/paid", sig)
	assert.Equal(t, 200, retry.Code, "the connection error was seen through the wrapper: the retry is served once")
	assert.Equal(t, 402, do(h, "GET", "/paid", sig).Code)
	assert.Equal(t, int32(2), hits.Load())
}

func TestFlushingWrapperThatBuffersIsFlushedBeforeTheErrorIsRead(t *testing.T) {
	f := startMock(t)
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	recorder := newDeadlineRecorder()
	serveWith(h, flushingWrapper{recorder}, signatureFor(t, h, testPreparedTx))
	assert.Equal(t, 200, recorder.Code)
	assert.JSONEq(t, `{"data":42}`, recorder.Body.String())
}

func TestWrapperWithoutUnwrapIsWarnedAboutOnce(t *testing.T) {
	blindFlushWarned.Store(false)
	t.Cleanup(func() { blindFlushWarned.Store(false) })
	f := startMock(t)
	logger, logs := capturingLogger()
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Logger = logger })
	sig := signatureFor(t, h, testPreparedTx)
	recorder := httptest.NewRecorder()
	serveWith(h, opaqueWrapper{recorder}, sig)
	require.Equal(t, 200, recorder.Code)
	assert.True(t, logsContain(logs, "level=WARN", "cannot see the connection", "counts as delivered", "Unwrap"))
	before := countOccurrences(logs.String(), "cannot see the connection")
	serveWith(h, opaqueWrapper{httptest.NewRecorder()}, signatureFor(t, h, "ee"+testPreparedTx[2:]))
	assert.Equal(t, before, countOccurrences(logs.String(), "cannot see the connection"), "once per process")
}

func countOccurrences(text, part string) int {
	count := 0
	for i := 0; i+len(part) <= len(text); i++ {
		if text[i:i+len(part)] == part {
			count++
		}
	}
	return count
}
