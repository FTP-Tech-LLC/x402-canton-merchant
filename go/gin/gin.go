// Package gin puts the Canton paid-route gate in front of a gin handler chain.
//
// Every request goes through the canton gate (net/http) and the handler chain
// runs inside it. The gate sees the chain's final answer before it settles:
// the adapter holds the answer, applies gin's own pending status (c.Status,
// WriteHeader without a body, a Redirect, ServeFile's 304) and refuses to
// settle when the chain recorded an error with c.Error (answered 500), aborted
// without an answer, changed a started 2xx into a 4xx or 5xx (Recovery inside
// the gate), or wrote to gin's own writer behind the gate's back
// (engine.HandleContext). A bare 3xx, 4xx or 5xx is not charged; a bare 2xx is
// an empty paid answer and is charged. A request gin routes to no handler never
// reaches NoRoute when it is a paid spelling.
//
// The gate flushes through the whole writer chain and reads the connection's
// error at the writer that owns it, so a failed write gives the claim back. That
// needs every ResponseWriter wrapper in front of the gate (gin middleware that
// replaces c.Writer) to implement Unwrap() http.ResponseWriter: without it the
// gate cannot see the connection and logs a warning. Compress at the proxy.
//
// Place gin.Recovery and any error-rendering middleware BEFORE the gate
// (outside it), call r.Use(Middleware(...)) once before routes and groups are
// created, and register a paid route only with the method of its key. A request
// that reaches a param or catch-all route or NoRoute on a paid path with a method
// that is not the key's is answered 404 here; Check is a startup sanity check.
// Pass the gate's own Middleware() to Middleware.
package gin

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"

	gingonic "github.com/gin-gonic/gin"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/flushwalk"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/paidpath"
)

// adapterKey marks a request this adapter gates (and only such a request), so
// the same gate registered twice passes it through instead of refusing it as
// "in progress"; a different gate still decides for its own routes.
type adapterKey struct{}

const (
	noWritten     = -1
	defaultStatus = http.StatusOK
	msgNoAnswer   = `{"error":"paid handler aborted without a response; nothing was charged"}`
	msgBadRequest = `{"error":"bad request"}`
	msgFailed     = `{"error":"paid handler failed; nothing was charged"}`
)

// ErrHijackNotSupported is returned by Hijack on a paid route: the gate holds the
// answer until the payment is settled, so the connection cannot be taken over.
var ErrHijackNotSupported = errors.New("canton gin: hijacking is not supported on a paid route")

// Middleware returns the gin handler that puts a canton gate (pass
// Gate.Middleware(), unchanged) in front of the routes registered after it.
// Register it once with Engine.Use, after gin.Recovery and before groups and
// routes; run Check before serving. A paid route runs inside the gate, and its
// answer is held until the payment is settled; a request the gate does not
// charge goes on through the chain.
func Middleware(gate func(http.Handler) http.Handler) gingonic.HandlerFunc {
	return func(c *gingonic.Context) {
		original := c.Writer
		originalRequest := c.Request
		outer := &outerWriter{ResponseWriter: original}
		entered := false

		if originalRequest.Context().Value(adapterKey{}) != nil {
			c.Next() // the gate is registered twice: the outer one already gates this request
			return
		}
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			entered = true
			if !canton.Verified(r.Context()) {
				// Not a paid request: the gate passed it through (it says so itself:
				// there is no verified payer). No marker, so another gate still decides.
				c.Request = r
				if wildcardRoute(c) && paidpath.OtherMethodGated(gate, r) {
					// A param or catch-all route (r.Any("/files/*filepath"), r.Match on
					// "/v1/:name") reached a paid path with a method that is not its key.
					c.AbortWithStatus(http.StatusNotFound)
					return
				}
				c.Next()
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), adapterKey{}, true))
			c.Request = r
			// The chain starts from gin's own status: 404 or 405 on a NoRoute or
			// NoMethod chain, 200 on a matched route.
			writer := &gateWriter{gate: w, original: original, size: noWritten, status: original.Status()}
			errorsBefore, sizeBefore := len(c.Errors), original.Size()
			c.Writer = writer
			c.Next()
			// engine.HandleContext resets c.Writer to gin's own writer: bytes that
			// reached the client before the gate decided must never be settled.
			writer.bypassed = original.Size() != sizeBefore
			bindFailed := false
			for _, recorded := range c.Errors[min(errorsBefore, len(c.Errors)):] {
				bindFailed = bindFailed || recorded.IsType(gingonic.ErrorTypeBind)
			}
			writer.finish(c.IsAborted(), len(c.Errors) > errorsBefore, bindFailed)
		})

		defer func() {
			c.Writer = original
			c.Request = originalRequest
		}()
		if c.FullPath() == "" {
			// gin matched no route. A path the gate treats as paid, in any spelling
			// ("//paid", "/./paid") and for any method, must not reach a NoRoute
			// handler that serves content by path (a file server, a proxy): answer
			// gin's own 404 without running it. Only an adapter on engine.Use is in
			// this chain.
			if paidpath.AnyMethodGated(gate, originalRequest) {
				c.Abort()
				return
			}
			c.Next()
			return
		}
		gate(next).ServeHTTP(outer, originalRequest)
		if !entered {
			c.Abort()
		}
	}
}

// outerWriter is gin's writer as the gate sees it. gin's own Flush asserts
// http.Flusher on the writer below it and panics behind a wrapper without
// Flush; here a flush that cannot happen is ErrNotSupported.
type outerWriter struct{ gingonic.ResponseWriter }

func (w *outerWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *outerWriter) FlushError() error {
	// No WriteHeaderNow here: a wrapper in front of the gate that holds the status
	// writes it in its own Flush, and gin's own Flush sends the header after it.
	// Every gin writer (gin's own, and any wrapper in front of the gate) has a
	// Flush() that drops the connection's error: walk to the writer that owns it.
	seen, err := flushwalk.Flush(w.ResponseWriter)
	if !seen {
		return http.ErrNotSupported
	}
	return err
}

// wildcardRoute is true for a route with a param or catch-all segment, which
// serves more than the one path it is registered for.
func wildcardRoute(c *gingonic.Context) bool { return paidpath.Wildcard(c.FullPath()) }

// gateWriter keeps the chain's answer until the chain is done, so the adapter
// can still refuse it (an error, an abort, a status changed after the answer
// started) before the gate sees it.
type gateWriter struct {
	gate     http.ResponseWriter
	original gingonic.ResponseWriter
	body     bytes.Buffer
	size     int
	status   int
	// failedLate: the chain asked for a 4xx/5xx after a 2xx answer started
	// (gin.Recovery inside the gate, an error renderer after a partial write).
	failedLate bool
	bypassed   bool
}

var _ gingonic.ResponseWriter = (*gateWriter)(nil)

func (w *gateWriter) Header() http.Header { return w.gate.Header() }

func (w *gateWriter) WriteHeader(code int) {
	if code > 0 && w.status != code {
		if w.Written() {
			if code >= http.StatusBadRequest && w.status < http.StatusMultipleChoices {
				w.failedLate = true
			}
			return
		}
		w.status = code
	}
}

func (w *gateWriter) WriteHeaderNow() {
	if !w.Written() {
		w.size = 0
	}
}

func (w *gateWriter) Write(data []byte) (int, error) {
	w.WriteHeaderNow()
	n, err := w.body.Write(data)
	w.size += n
	return n, err
}

func (w *gateWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *gateWriter) Status() int                       { return w.status }
func (w *gateWriter) Size() int                         { return w.size }
func (w *gateWriter) Written() bool                     { return w.size != noWritten }
func (w *gateWriter) Flush()                            { w.WriteHeaderNow() }
func (w *gateWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, ErrHijackNotSupported
}
func (w *gateWriter) CloseNotify() <-chan bool { return w.original.CloseNotify() }
func (w *gateWriter) Pusher() http.Pusher      { return nil }

// finish hands the chain's answer to the gate, as gin's engine does with its
// final WriteHeaderNow, or replaces it with a refusal that is never charged.
func (w *gateWriter) finish(aborted, newErrors, bindFailed bool) {
	success := w.status >= http.StatusOK && w.status < http.StatusMultipleChoices
	switch {
	case aborted && !w.Written() && w.status == defaultStatus:
		w.refuse(msgNoAnswer)
	case w.bypassed || w.failedLate || (success && newErrors):
		w.refuse(msgFailed)
	case bindFailed && w.status >= http.StatusBadRequest && w.status < http.StatusInternalServerError:
		// gin's c.Bind* answered 400 itself. Anything the handler wrote after ignoring
		// that error is paid content in an uncharged answer: never send it.
		header := w.gate.Header()
		clear(header) // headers set after the ignored error belong to the paid answer
		header.Set("Content-Type", "application/json")
		w.gate.WriteHeader(w.status)
		_, _ = w.gate.Write([]byte(msgBadRequest))
	default:
		w.gate.WriteHeader(w.status)
		_, _ = w.gate.Write(w.body.Bytes())
	}
}

func (w *gateWriter) refuse(message string) {
	header := w.gate.Header()
	clear(header) // the handler's headers were meant for a paid answer
	header.Set("Content-Type", "application/json")
	w.gate.WriteHeader(http.StatusInternalServerError)
	_, _ = w.gate.Write([]byte(message))
}
