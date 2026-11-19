package echo_test

// Round 3 probes: Check's other-method scan and the e.Pre warning.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/labstack/gommon/log"
	"github.com/stretchr/testify/assert"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// A correct setup: one paid GET route, the gate on e.Use, and an e.Any health
// route elsewhere (or any OPTIONS route, as CORS setups have). echo answers
// OPTIONS on every existing path itself (204 + Allow); no handler runs.
func TestR3CheckFalseFailsOnEchosOwnOptionsAnswer(t *testing.T) {
	for _, tc := range []struct {
		name     string
		register func(e *echolib.Echo)
	}{
		{"e.Any elsewhere", func(e *echolib.Echo) {
			e.Any("/healthz", func(c echolib.Context) error { return c.String(200, "ok") })
		}},
		{"an OPTIONS route elsewhere", func(e *echolib.Echo) {
			e.OPTIONS("/upload", func(c echolib.Context) error { return c.NoContent(204) })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testkit.StartFacilitator(t)
			g := testkit.NewGate(t, f)
			var hits atomic.Int32
			e := r3Echo()
			e.Use(middleware.Recover())
			e.Use(cantonecho.Middleware(g.Middleware()))
			e.GET("/paid", func(c echolib.Context) error { hits.Add(1); return c.String(200, "PAID") })
			tc.register(e)

			options := testkit.Do(e, "OPTIONS", "/paid", "")
			err := cantonecho.Check(e, testkit.Routes())
			t.Logf("OPTIONS /paid unpaid: %d allow=%q body=%q hits=%d; Check=%v",
				options.Code, options.Header().Get("Allow"), options.Body.String(), hits.Load(), err)
			assert.Zero(t, hits.Load(), "no handler ran: echo answered OPTIONS itself")
			assert.NoError(t, err, "a correct setup must pass Check")
		})
	}
}

// The e.Pre warning is keyed on an empty c.Path(). On a correct setup (gate on
// e.Use), does a paid request for an unrouted spelling trigger it? The warning
// is once per process: run alone, -run '^TestR3PreWarningOnACorrectSetup$'.
func TestR3PreWarningOnACorrectSetup(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := r3Echo()
	var logs bytes.Buffer
	e.Logger.SetOutput(&logs)
	e.Logger.SetLevel(log.DEBUG) // so a Warn is visible at all
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return c.String(200, "PAID") })
	e.GET("/other", func(c echolib.Context) error { return c.String(200, "other") })
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	for _, target := range []string{"/paid/", "//paid", "/%70aid"} {
		w := testkit.Do(e, "GET", target, sig)
		t.Logf("%s: %d %q", target, w.Code, w.Body.String())
	}
	t.Logf("echo log: %q", logs.String())
	assert.False(t, strings.Contains(logs.String(), "e.Pre"), "a correct e.Use setup must not be told it runs in e.Pre")
}

// The gate in e.Pre (the setup the warning is for), on echo's default logger:
// echo.New() sets the level to ERROR, so the Warn is dropped, and the
// once-per-process flag is spent. Run alone:
// -run '^TestR3PreWarningIsDroppedAtEchosDefaultLogLevel$'.
func TestR3PreWarningIsDroppedAtEchosDefaultLogLevel(t *testing.T) {
	cantonecho.ResetPreWarned()
	t.Cleanup(cantonecho.ResetPreWarned)
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := echolib.New()
	e.HideBanner, e.HidePort = true, true
	var logs bytes.Buffer
	e.Logger.SetOutput(&logs) // the default level, as in examples/echo
	e.Pre(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return c.String(200, "PAID") })
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	w := testkit.Do(e, "GET", "/paid", sig)
	t.Logf("paid request with the gate in e.Pre: %d %q verify=%d settles=%d; Check=%v; echo log: %q",
		w.Code, w.Body.String(), f.VerifyCalls.Load(), f.SettleCalls.Load(), cantonecho.Check(e, testkit.Routes()), logs.String())
	assert.Contains(t, logs.String(), "e.Pre", "the merchant must see the e.Pre warning")
}

// A search endpoint that takes GET with a query or POST with a JSON body, keyed
// for GET only and registered with e.Match: README says Match serves the other
// methods free, and Check is the net for it. Check's POST has no body, so the
// handler answers 400 and Check passes; a real POST with a body is served free.
func TestR3CheckMissesASecondMethodThatNeedsABody(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f, routesFor("GET /search"))
	var hits atomic.Int32
	e := r3Echo()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.Match([]string{http.MethodGet, http.MethodPost}, "/search", func(c echolib.Context) error {
		var q struct {
			Query string `json:"q" query:"q"`
		}
		if err := c.Bind(&q); err != nil || q.Query == "" {
			return echolib.NewHTTPError(http.StatusBadRequest, "q is required")
		}
		hits.Add(1)
		return c.String(200, "PAID RESULTS for "+q.Query)
	})

	checkErr := cantonecho.Check(e, map[string]canton.Route{"GET /search": {PayTo: testkit.PayTo, Price: canton.CC("0.01")}})
	req := httptest.NewRequest(http.MethodPost, "/search", strings.NewReader(`{"q":"canton"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	t.Logf("Check=%v; unpaid POST /search with a body: %d %q hits=%d", checkErr, w.Code, w.Body.String(), hits.Load())
	assert.Error(t, checkErr, "Check must report e.Match serving POST on a GET-keyed paid path")
}
