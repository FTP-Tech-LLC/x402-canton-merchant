package gin_test

import (
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func init() {
	gingonic.SetMode(gingonic.TestMode)
	gingonic.DefaultErrorWriter = io.Discard
}

type ctx struct{ c *gingonic.Context }

func (x ctx) Request() *http.Request       { return x.c.Request }
func (x ctx) SetHeader(key, value string)  { x.c.Header(key, value) }
func (x ctx) Respond(status int, b string) { x.c.String(status, "%s", b) }
func (x ctx) Redirect(status int, u string) {
	x.c.Redirect(status, u)
}
func (x ctx) Fail(status int) { x.c.AbortWithStatus(status) }
func (x ctx) Panic()          { panic("handler bug") }
func (x ctx) RespondNoContentWithBody() {
	x.c.Writer.WriteHeader(http.StatusNoContent)
	_, _ = x.c.Writer.Write([]byte("body"))
}

func engine(mw func(http.Handler) http.Handler, paid ...gingonic.HandlerFunc) *gingonic.Engine {
	r := gingonic.New()
	r.Use(gingonic.Recovery()) // outside the gate: a panic is never settled
	r.Use(cantongin.Middleware(mw))
	r.GET("/paid", paid...)
	r.GET("/free", func(c *gingonic.Context) { c.String(200, "free") })
	return r
}

func TestMoneyMatrixThroughGin(t *testing.T) {
	testkit.RunMatrix(t, func(_ *testing.T, mw func(http.Handler) http.Handler, script func(testkit.Ctx)) http.Handler {
		return engine(mw, func(c *gingonic.Context) { script(ctx{c}) })
	})
}

func setup(t *testing.T, build func(mw func(http.Handler) http.Handler) http.Handler) (*testkit.Facilitator, http.Handler) {
	f := testkit.StartFacilitator(t)
	return f, build(testkit.NewGate(t, f).Middleware())
}

func TestGateAnswerStopsTheGinChain(t *testing.T) {
	var before, handler atomic.Int32
	f, h := setup(t, func(mw func(http.Handler) http.Handler) http.Handler {
		r := gingonic.New()
		r.Use(cantongin.Middleware(mw))
		r.GET("/paid", func(c *gingonic.Context) { before.Add(1) }, func(c *gingonic.Context) { handler.Add(1) })
		return r
	})
	assert.Equal(t, 402, testkit.Do(h, "GET", "/paid", "").Code)
	f.OnVerify(func(_ int32, w http.ResponseWriter) { testkit.Reply(w, 503, map[string]string{"error": "scan_busy"}) })
	sig := testkit.SignatureFor(t, h, testkit.PreparedTx)
	assert.Equal(t, 503, testkit.Do(h, "GET", "/paid", sig).Code)
	assert.Zero(t, before.Load(), "no later handler runs when the gate answered itself")
	assert.Zero(t, handler.Load())
}

func TestWriterSeenByAGinHandlerBehavesLikeGins(t *testing.T) {
	var seen struct {
		writtenBefore, writtenAfter bool
		statusBefore, statusAfter   int
		sizeBefore, sizeAfter       int
		hijackErr                   error
		pusher                      http.Pusher
	}
	_, h := setup(t, func(mw func(http.Handler) http.Handler) http.Handler {
		r := gingonic.New()
		r.Use(cantongin.Middleware(mw))
		r.GET("/paid", func(c *gingonic.Context) {
			seen.writtenBefore, seen.statusBefore, seen.sizeBefore = c.Writer.Written(), c.Writer.Status(), c.Writer.Size()
			c.Writer.WriteHeader(201)
			_, _ = c.Writer.WriteString("hello")
			seen.writtenAfter, seen.statusAfter, seen.sizeAfter = c.Writer.Written(), c.Writer.Status(), c.Writer.Size()
			_, _, seen.hijackErr = c.Writer.Hijack()
			seen.pusher = c.Writer.Pusher()
			c.Writer.Flush() // must not panic or stream
		})
		return r
	})
	w := testkit.Do(h, "GET", "/paid", testkit.SignatureFor(t, h, testkit.PreparedTx))
	assert.Equal(t, 201, w.Code)
	assert.Equal(t, "hello", w.Body.String())
	assert.False(t, seen.writtenBefore)
	assert.Equal(t, 200, seen.statusBefore)
	assert.Equal(t, -1, seen.sizeBefore)
	assert.True(t, seen.writtenAfter)
	assert.Equal(t, 201, seen.statusAfter)
	assert.Equal(t, 5, seen.sizeAfter)
	assert.ErrorIs(t, seen.hijackErr, cantongin.ErrHijackNotSupported)
	assert.Nil(t, seen.pusher)
}

func TestGinWriterIsRestoredAfterTheGate(t *testing.T) {
	var outside, inside, after gingonic.ResponseWriter
	_, h := setup(t, func(mw func(http.Handler) http.Handler) http.Handler {
		r := gingonic.New()
		r.Use(func(c *gingonic.Context) { outside = c.Writer; c.Next(); after = c.Writer })
		r.Use(cantongin.Middleware(mw))
		r.GET("/paid", func(c *gingonic.Context) { inside = c.Writer; c.String(200, "ok") })
		return r
	})
	require.Equal(t, 200, testkit.Do(h, "GET", "/paid", testkit.SignatureFor(t, h, testkit.PreparedTx)).Code)
	assert.NotSame(t, outside, inside, "the handler writes into the gate's writer")
	assert.Same(t, outside, after, "gin's own writer is back after the request")
}

func TestHandlerThatWritesNothingIsAnEmpty200(t *testing.T) {
	// gin's own final WriteHeaderNow must not send a status before or instead of the gate's answer.
	f, h := setup(t, func(mw func(http.Handler) http.Handler) http.Handler {
		r := gingonic.New()
		r.Use(cantongin.Middleware(mw))
		r.GET("/paid", func(c *gingonic.Context) {})
		return r
	})
	sig := testkit.SignatureFor(t, h, testkit.PreparedTx)
	w := testkit.Do(h, "GET", "/paid", sig)
	assert.Equal(t, 200, w.Code)
	assert.NotEmpty(t, w.Header().Get("PAYMENT-RESPONSE"))
	assert.Equal(t, int32(1), f.SettleCalls.Load())
}

func TestAbortWithoutAnAnswerIsNotSettled(t *testing.T) {
	f, h := setup(t, func(mw func(http.Handler) http.Handler) http.Handler {
		r := gingonic.New()
		r.Use(cantongin.Middleware(mw))
		r.GET("/paid", func(c *gingonic.Context) { c.Abort() })
		return r
	})
	w := testkit.Do(h, "GET", "/paid", testkit.SignatureFor(t, h, testkit.PreparedTx))
	assert.Equal(t, 500, w.Code)
	assert.Contains(t, w.Body.String(), "nothing was charged")
	assert.Zero(t, f.SettleCalls.Load())
}

func TestRecoveryInsideTheGateAnswers500WhichIsNotSettled(t *testing.T) {
	f, h := setup(t, func(mw func(http.Handler) http.Handler) http.Handler {
		r := gingonic.New()
		r.Use(cantongin.Middleware(mw))
		r.Use(gingonic.Recovery())
		r.GET("/paid", func(c *gingonic.Context) { panic("bug") })
		return r
	})
	w := testkit.Do(h, "GET", "/paid", testkit.SignatureFor(t, h, testkit.PreparedTx))
	assert.Equal(t, 500, w.Code)
	assert.Zero(t, f.SettleCalls.Load())
}

func TestPaidRouteInAGroupIsKeyedByItsFullPath(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f, func(c *canton.Config) {
		c.Routes = map[string]canton.Route{"GET /api/v1/data": {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}
	})
	r := gingonic.New()
	v1 := r.Group("/api").Group("/v1")
	v1.Use(cantongin.Middleware(g.Middleware()))
	v1.GET("/data", func(c *gingonic.Context) { c.String(200, "paid") })
	v1.GET("/open", func(c *gingonic.Context) { c.String(200, "open") })

	assert.Equal(t, 402, testkit.Do(r, "GET", "/api/v1/data", "").Code)
	assert.Equal(t, "open", testkit.Do(r, "GET", "/api/v1/open", "").Body.String())
	sig := testkit.SignatureForPath(t, r, "/api/v1/data", testkit.PreparedTx)
	w := testkit.Do(r, "GET", "/api/v1/data", sig)
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "paid", w.Body.String())
}

func TestPathParameterAndWildcardKeysAreRefused(t *testing.T) {
	f := testkit.StartFacilitator(t)
	for _, key := range []string{"GET /items/:id", "GET /files/*path", "GET /items/{id}"} {
		_, err := canton.NewGate(t.Context(), canton.Config{FacilitatorURL: f.URL,
			Routes: map[string]canton.Route{key: {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}})
		assert.Error(t, err, key)
	}
}
