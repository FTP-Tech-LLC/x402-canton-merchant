package echo_test

import (
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// ctx is the matrix's framework-neutral handler context over an echo context.
// Respond and Fail record an error that the echo handler returns.
type ctx struct {
	c   echolib.Context
	err *error
}

func (x ctx) Request() *http.Request      { return x.c.Request() }
func (x ctx) SetHeader(key, value string) { x.c.Response().Header().Set(key, value) }
func (x ctx) Respond(status int, b string) {
	*x.err = x.c.String(status, b)
}
func (x ctx) Redirect(status int, u string) { *x.err = x.c.Redirect(status, u) }
func (x ctx) Fail(status int)               { *x.err = echolib.NewHTTPError(status) }
func (x ctx) Panic()                        { panic("handler bug") }
func (x ctx) RespondNoContentWithBody() {
	x.c.Response().WriteHeader(http.StatusNoContent)
	_, _ = x.c.Response().Write([]byte("body"))
}

func engine(mw func(http.Handler) http.Handler, paid echolib.HandlerFunc) *echolib.Echo {
	e := echolib.New()
	e.Logger.SetOutput(io.Discard)
	e.HideBanner, e.HidePort = true, true
	e.Use(middleware.Recover()) // outside the gate: a panic is never settled
	e.Use(cantonecho.Middleware(mw))
	e.GET("/paid", paid)
	e.GET("/free", func(c echolib.Context) error { return c.String(200, "free") })
	return e
}

func TestMoneyMatrixThroughEcho(t *testing.T) {
	testkit.RunMatrix(t, func(_ *testing.T, mw func(http.Handler) http.Handler, script func(testkit.Ctx)) http.Handler {
		return engine(mw, func(c echolib.Context) error {
			var err error
			script(ctx{c: c, err: &err})
			return err
		})
	})
}

func setup(t *testing.T, build func(mw func(http.Handler) http.Handler) http.Handler) (*testkit.Facilitator, http.Handler) {
	f := testkit.StartFacilitator(t)
	return f, build(testkit.NewGate(t, f).Middleware())
}

func newEcho(mw func(http.Handler) http.Handler, handler echolib.HandlerFunc) *echolib.Echo {
	e := echolib.New()
	e.Logger.SetOutput(io.Discard)
	e.HideBanner, e.HidePort = true, true
	e.Use(cantonecho.Middleware(mw))
	e.GET("/paid", handler)
	return e
}

func TestHandlerErrorAfterA200WasWrittenIsRenderedAndNotSettled(t *testing.T) {
	var hits atomic.Int32
	f, h := setup(t, func(mw func(http.Handler) http.Handler) http.Handler {
		return newEcho(mw, func(c echolib.Context) error {
			hits.Add(1)
			_ = c.String(200, "partial paid content") // echo would treat this as committed
			return errors.New("the handler failed after writing")
		})
	})
	sig := testkit.SignatureFor(t, h, testkit.PreparedTx)
	w := testkit.Do(h, "GET", "/paid", sig)
	assert.Equal(t, 500, w.Code, "the error is rendered, not answered after a 200 went out")
	assert.NotContains(t, w.Body.String(), "partial paid content")
	assert.Zero(t, f.SettleCalls.Load(), "a returned error is never settled")
	assert.Empty(t, w.Header().Get("PAYMENT-RESPONSE"))
}

func TestHandlerErrorBeforeAnyWriteIsEchosErrorAnswer(t *testing.T) {
	f, h := setup(t, func(mw func(http.Handler) http.Handler) http.Handler {
		return newEcho(mw, func(c echolib.Context) error { return echolib.NewHTTPError(http.StatusTeapot, "short and stout") })
	})
	w := testkit.Do(h, "GET", "/paid", testkit.SignatureFor(t, h, testkit.PreparedTx))
	assert.Equal(t, http.StatusTeapot, w.Code)
	assert.Contains(t, w.Body.String(), "short and stout")
	assert.Zero(t, f.SettleCalls.Load())
}

func TestCustomHTTPErrorHandlerRendersTheError(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := newEcho(g.Middleware(), func(c echolib.Context) error { return errors.New("boom") })
	e.HTTPErrorHandler = func(err error, c echolib.Context) {
		_ = c.JSON(http.StatusBadGateway, map[string]string{"custom": err.Error()})
	}
	w := testkit.Do(e, "GET", "/paid", testkit.SignatureFor(t, e, testkit.PreparedTx))
	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.JSONEq(t, `{"custom":"boom"}`, w.Body.String())
	assert.Zero(t, f.SettleCalls.Load())
}

func TestEchoResponseReportsTheGatesAnswer(t *testing.T) {
	var status, status402 int
	var committed bool
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := echolib.New()
	e.Logger.SetOutput(io.Discard)
	e.Use(func(next echolib.HandlerFunc) echolib.HandlerFunc { // a logger outside the gate
		return func(c echolib.Context) error {
			err := next(c)
			status, committed = c.Response().Status, c.Response().Committed
			return err
		}
	})
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return c.String(200, "ok") })

	testkit.Do(e, "GET", "/paid", "")
	status402 = status
	assert.Equal(t, 402, status402, "a gate answer shows up in echo's Response status")
	testkit.Do(e, "GET", "/paid", testkit.SignatureFor(t, e, testkit.PreparedTx))
	assert.Equal(t, 200, status)
	assert.True(t, committed)
}

func TestFlushInsideAnEchoHandlerDoesNotPanicOrStream(t *testing.T) {
	_, h := setup(t, func(mw func(http.Handler) http.Handler) http.Handler {
		return newEcho(mw, func(c echolib.Context) error {
			_ = c.String(200, "ok")
			c.Response().Flush()
			return nil
		})
	})
	w := testkit.Do(h, "GET", "/paid", testkit.SignatureFor(t, h, testkit.PreparedTx))
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "ok", w.Body.String())
	assert.NotEmpty(t, w.Header().Get("PAYMENT-RESPONSE"), "settled before anything was sent")
}

func TestRecoverInsideTheGateRendersAnErrorWhichIsNotSettled(t *testing.T) {
	f, h := setup(t, func(mw func(http.Handler) http.Handler) http.Handler {
		e := echolib.New()
		e.Logger.SetOutput(io.Discard)
		e.Use(cantonecho.Middleware(mw))
		e.Use(middleware.Recover())
		e.GET("/paid", func(c echolib.Context) error { panic("bug") })
		return e
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
	e := echolib.New()
	e.Logger.SetOutput(io.Discard)
	v1 := e.Group("/api").Group("/v1")
	v1.Use(cantonecho.Middleware(g.Middleware()))
	v1.GET("/data", func(c echolib.Context) error { return c.String(200, "paid") })
	v1.GET("/open", func(c echolib.Context) error { return c.String(200, "open") })

	assert.Equal(t, 402, testkit.Do(e, "GET", "/api/v1/data", "").Code)
	assert.Equal(t, "open", testkit.Do(e, "GET", "/api/v1/open", "").Body.String())
	w := testkit.Do(e, "GET", "/api/v1/data", testkit.SignatureForPath(t, e, "/api/v1/data", testkit.PreparedTx))
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "paid", w.Body.String())
}

func TestPathParameterAndWildcardKeysAreRefused(t *testing.T) {
	f := testkit.StartFacilitator(t)
	for _, key := range []string{"GET /items/:id", "GET /files/*", "GET /items/{id}"} {
		_, err := canton.NewGate(t.Context(), canton.Config{FacilitatorURL: f.URL,
			Routes: map[string]canton.Route{key: {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}})
		assert.Error(t, err, key)
	}
}

func TestMiddlewareRestoresEchoStateAfterAPanic(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	var original http.ResponseWriter
	var after http.ResponseWriter
	e := echolib.New()
	e.Logger.SetOutput(io.Discard)
	e.Use(func(next echolib.HandlerFunc) echolib.HandlerFunc {
		return func(c echolib.Context) (err error) {
			original = c.Response().Writer
			defer func() { _ = recover(); after = c.Response().Writer }()
			return next(c)
		}
	})
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { panic("bug") })
	require.NotPanics(t, func() {
		testkit.Do(e, "GET", "/paid", testkit.SignatureFor(t, e, testkit.PreparedTx))
	})
	assert.Same(t, original, after, "echo's writer is back even after a panic")
}
