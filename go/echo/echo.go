// Package echo puts the Canton paid-route gate in front of an echo handler.
//
// Every request goes through the canton gate (net/http) and the handler runs
// inside it, on its own echo Response. A returned error is rendered through
// echo's HTTPErrorHandler into a fresh buffer before the gate decides; if the
// error handler renders nothing or a status below 400 (a redirect included),
// the answer is a 500 that is not charged. A returned error is therefore never
// settled and never answered after a 2xx went out.
//
// The gate flushes through the whole writer chain and reads the connection's
// error at the writer that owns it, so a failed write gives the claim back. That
// needs every ResponseWriter wrapper in front of the gate to implement
// Unwrap() http.ResponseWriter: echo's Gzip and BodyDump drop the connection's
// error and cannot be fully seen through, so keep them off paid routes (use their
// Skipper) and compress at the proxy; without a way through, the gate logs a
// warning.
//
// Register echo's Recover, Logger and any middleware that renders errors or
// recovers panics BEFORE Middleware (outside the gate: the first e.Use is the
// outermost). Register Middleware once with e.Use (not e.Pre), before groups and
// routes are created, and register a paid route only with the method of its key.
// Do not put Gzip or another response wrapper inside the gate on a paid route.
// Over HTTP/2 straight to the app, a failed write whose connection still looks
// open on the read side can count as delivered (net/http cannot wait for the
// socket flush): put a proxy that speaks HTTP/1.1 to the app in front.
// Check verifies the registration rules at startup.
package echo

import (
	"context"
	"net/http"
	"reflect"
	"sync/atomic"

	echolib "github.com/labstack/echo/v4"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/flushwalk"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/paidpath"
)

// adapterKey marks a request this adapter gates (only such a request), so the
// same gate registered twice (on a group, say) passes it through instead of
// refusing it as "payment in progress"; a different gate still decides.
type adapterKey struct{}

// preWarned makes the e.Pre warning log once per process.
var preWarned atomic.Bool

const msgNoAnswer = `{"error":"paid handler returned an error without an error answer; nothing was charged"}`

// Middleware returns the echo middleware for a gate.
func Middleware(gate func(http.Handler) http.Handler) echolib.MiddlewareFunc {
	return func(next echolib.HandlerFunc) echolib.HandlerFunc {
		return func(c echolib.Context) error {
			response := c.Response()
			originalRequest := c.Request()
			if originalRequest.Context().Value(adapterKey{}) != nil {
				return next(c) // the gate is registered twice: the outer one already gates this request
			}
			out := &gateResponse{Response: response}
			var handlerErr error
			entered := false

			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				entered = true
				if !canton.Verified(r.Context()) {
					// Not a paid request: the gate passed it through (it says so itself:
					// there is no verified payer). No marker, so another gate still decides.
					c.SetRequest(r)
					if reachesPaidPath(c, gate, r) {
						handlerErr = echolib.ErrNotFound
						return
					}
					handlerErr = next(c)
					return
				}
				r = r.WithContext(context.WithValue(r.Context(), adapterKey{}, true))
				c.SetRequest(r)
				runPaid(c, response, w, next)
			})

			defer c.SetRequest(originalRequest)
			gate(inner).ServeHTTP(out, originalRequest)
			if !entered {
				return nil
			}
			return handlerErr
		}
	}
}

// gateResponse is echo's Response as the gate's writer. echo's Response.Flush
// panics when the writer under it cannot flush (http.TimeoutHandler, a plain
// status recorder); FlushError reports that instead, as net/http does.
type gateResponse struct{ *echolib.Response }

func (w *gateResponse) FlushError() error {
	// echo's Response.Flush and the Flush of Gzip and BodyDump drop the connection's
	// error: walk to the writer that owns it.
	seen, err := flushwalk.Flush(w.Writer)
	if !seen {
		return http.ErrNotSupported
	}
	return err
}

func newResponse(w http.ResponseWriter, e *echolib.Echo) *echolib.Response {
	r := echolib.NewResponse(w, e)
	r.Status = http.StatusOK // as echo's own reset
	return r
}

// runPaid runs the handler against a private echo Response (its own writer,
// hooks and Committed), renders a returned error into a fresh one, and hands
// the finished answer to the gate's writer.
func runPaid(c echolib.Context, response *echolib.Response, gateWriter http.ResponseWriter, next echolib.HandlerFunc) {
	defer c.SetResponse(response) // also after a panic
	buffer := &bufferWriter{header: gateWriter.Header()}
	if c.Path() == "" && hasExactRoute(c.Echo(), c.Request()) && preWarned.CompareAndSwap(false, true) {
		// Error, not Warn: echo.New() logs at ERROR and above.
		c.Logger().Error("canton: the gate runs before routing (e.Pre): paid requests cannot be delivered; register it with e.Use")
	}
	if c.Path() != c.Request().URL.Path {
		// The gate charges this spelling ("/paid/", "//paid", "/%70aid"), but echo did
		// not route it to the handler registered under the paid path: never charge
		// another handler's answer (a catch-all, a 404).
		buffer.header.Set("Content-Type", "application/json")
		buffer.WriteHeader(http.StatusNotFound)
		_, _ = buffer.Write([]byte(`{"error":"not found; nothing was charged"}`))
		buffer.replay(gateWriter)
		return
	}
	c.SetResponse(newResponse(buffer, c.Echo()))
	if err := next(c); err != nil {
		if buffer.status != 0 {
			clear(buffer.header) // the answer the handler started is replaced, headers included
		}
		buffer.reset()
		c.SetResponse(newResponse(buffer, c.Echo())) // a writer no inner middleware has wrapped or closed
		c.Error(err)
		if buffer.status < http.StatusBadRequest { // nothing rendered, or an error rendered as 1xx/2xx/3xx
			clear(buffer.header)
			buffer.reset()
			buffer.header.Set("Content-Type", "application/json")
			buffer.WriteHeader(http.StatusInternalServerError)
			_, _ = buffer.Write([]byte(msgNoAnswer))
		}
	}
	buffer.replay(gateWriter)
}

type bufferWriter struct {
	header http.Header
	status int
	body   []byte
}

func (b *bufferWriter) Header() http.Header { return b.header }

func (b *bufferWriter) WriteHeader(code int) {
	if b.status != 0 || (code >= http.StatusContinue && code < http.StatusOK) {
		return
	}
	if code == 0 {
		code = -1 // net/http panics on WriteHeader(0): never a silent 200
	}
	b.status = code
}

func (b *bufferWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	b.body = append(b.body, p...)
	return len(p), nil
}

func (b *bufferWriter) Flush() {}

func (b *bufferWriter) reset() { b.status, b.body = 0, nil }

func (b *bufferWriter) replay(w http.ResponseWriter) {
	if b.status != 0 {
		w.WriteHeader(b.status)
	}
	if len(b.body) > 0 {
		_, _ = w.Write(b.body)
	}
}

// hasExactRoute is true when the request's router finds a route for exactly this
// method and path: an empty c.Path() then means echo has not routed yet (e.Pre).
// It asks the router (as echo does per request), not echo's route map.
func hasExactRoute(e *echolib.Echo, r *http.Request) bool {
	router := e.Router()
	if hostRouter, found := e.Routers()[r.Host]; found {
		router = hostRouter
	}
	probe := e.NewContext(r, nil)
	router.Find(r.Method, echolib.GetPath(r), probe)
	return probe.Path() == echolib.GetPath(r) && probe.Get(echolib.ContextKeyHeaderAllow) == nil
}

// hasOwnRoute decides "an exact route of the merchant's own" from echo's own
// routing result: echo sets ContextKeyHeaderAllow only when it found the path but
// no route for the method (its 405, and its own OPTIONS answer). Nothing is read
// from echo's route map while serving (it must not be read during registration),
// and no state is shared between echo instances.
func hasOwnRoute(c echolib.Context) bool {
	if c.Get(echolib.ContextKeyHeaderAllow) != nil {
		return false
	}
	// echo answers a RouteNotFound route on the path (e.Group(prefix, mw) adds one
	// at the prefix) before its 405: what the router gives a method no route has
	// is that route; the same handler means this request matched no route of its own.
	e := c.Echo()
	router := e.Router()
	if hostRouter, found := e.Routers()[c.Request().Host]; found {
		router = hostRouter
	}
	probe := e.NewContext(c.Request(), nil)
	router.Find("CANTON-NO-SUCH-METHOD", echolib.GetPath(c.Request()), probe)
	return probe.Path() != c.Path() || reflect.ValueOf(probe.Handler()).Pointer() != reflect.ValueOf(c.Handler()).Pointer()
}

// SkipPaidPaths is a Skipper for middleware that serves content by path
// (middleware.Static, a proxy) and sits inside the gate: it skips every request
// for a path the gate charges or refuses, whatever the method. The adapter's
// guard cannot stop an inner middleware on an exact route of your own for
// another method, so give such middleware this Skipper or keep paid files out of
// its root:
//
//	e.Use(middleware.StaticWithConfig(middleware.StaticConfig{Root: "public", Skipper: cantonecho.SkipPaidPaths(paid.Middleware())}))
func SkipPaidPaths(gate func(http.Handler) http.Handler) func(echolib.Context) bool {
	return func(c echolib.Context) bool { return paidpath.AnyMethodGated(gate, c.Request()) }
}

// reachesPaidPath is the runtime guard: a request the gate passed through as
// free that echo sends to a param or catch-all route, to RouteNotFound or to its
// own 404/405 (Static, a file server inside the gate) must not get a paid path
// under a method that is not the key's. An exact route of the merchant's own
// (an ingest on POST /paid) is left alone: it is a handler of its own.
func reachesPaidPath(c echolib.Context, gate func(http.Handler) http.Handler, r *http.Request) bool {
	path := c.Path()
	if path != "" && !paidpath.Wildcard(path) && hasOwnRoute(c) {
		return false
	}
	// TRADE-OFF: up to five cheap gate probes per free request on a param, catch-all
	// or unrouted path; exact routes skip them.
	return paidpath.OtherMethodGated(gate, r)
}
