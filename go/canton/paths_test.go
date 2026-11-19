package canton

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Rule 15: every spelling of a paid path is charged, refused, or sent back
// through the gate. Tested in front of a real http.ServeMux.

func serveRaw(h http.Handler, method, target string) *httptest.ResponseRecorder {
	parsed, err := url.ParseRequestURI(target)
	if err != nil {
		panic(err)
	}
	r := httptest.NewRequest(method, "/", nil)
	r.URL, r.RequestURI = parsed, target
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// outcome describes what an unpaid request to a spelling ends up as, following
// one redirect from the router back through the gate.
func outcome(t *testing.T, h http.Handler, method, target string) string {
	t.Helper()
	w := serveRaw(h, method, target)
	require.NotContains(t, w.Body.String(), "PAID-CONTENT", "%s %s served paid content without payment", method, target)
	switch {
	case w.Code == 402:
		return "402 charged"
	case w.Code == 404:
		return "404 not served"
	case w.Code >= 300 && w.Code < 400:
		location := w.Header().Get("Location")
		again := serveRaw(h, method, location)
		require.NotContains(t, again.Body.String(), "PAID-CONTENT")
		require.Contains(t, []int{402, 404}, again.Code, "redirect %s -> %s must come back through the gate", target, location)
		return w.Result().Status + " to " + location + " then " + again.Result().Status
	default:
		t.Fatalf("%s %s answered %d without payment", method, target, w.Code)
		return ""
	}
}

func TestPathSpellingsAreChargedOrNotServed(t *testing.T) {
	f := startMock(t)
	mux := http.NewServeMux()
	var hits atomic.Int32
	mux.HandleFunc("GET /paid", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("PAID-CONTENT"))
	})
	mux.HandleFunc("GET /free", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("free")) })
	h := newGate(t, f, mux.ServeHTTP)

	spellings := []struct{ method, target string }{
		{"GET", "/paid"},
		{"GET", "/paid/"},
		{"GET", "/PAID"},
		{"GET", "/Paid"},
		{"HEAD", "/paid"},
		{"GET", "/%70aid"},
		{"GET", "/%50AID"},
		{"GET", "/./paid"},
		{"GET", "//paid"},
		{"GET", "/paid/."},
		{"GET", "/x/../paid"},
		{"GET", "/%2e/paid"},
		{"GET", "/paid%2F"},
		{"GET", "/paid;x=1"},
		{"GET", "/paid?x=1"},
		{"GET", "/paid#frag"},
		{"GET", "/%2Fpaid"},
		{"GET", "///paid"},
	}
	for _, spelling := range spellings {
		t.Logf("%-5s %-14s -> %s", spelling.method, spelling.target, outcome(t, h, spelling.method, spelling.target))
	}
	assert.Equal(t, int32(0), hits.Load(), "no unpaid request reached the paid handler")
	assert.Equal(t, int32(0), f.verifyCalls.Load())

	assert.Equal(t, 200, serveRaw(h, "GET", "/free").Code, "free routes are untouched")
	assert.Equal(t, "402 charged", outcome(t, h, "GET", "/paid"))
	assert.Equal(t, "402 charged", outcome(t, h, "HEAD", "/paid"))
	assert.Equal(t, "402 charged", outcome(t, h, "GET", "/paid/"), "trailing slash is gated by the SDK")
	assert.Equal(t, "402 charged", outcome(t, h, "GET", "/%70aid"), "percent escapes decode to the paid path")
	assert.Equal(t, "402 charged", outcome(t, h, "GET", "//paid"))
}

func TestCaseAndDotSegmentSpellingsAreRefusedByTheGateItself(t *testing.T) {
	// A router that folds case or resolves dot segments would serve the paid
	// handler for these. The gate answers 404 before the router sees them.
	f := startMock(t)
	var reached atomic.Int32
	h := newGate(t, f, func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		_, _ = w.Write([]byte("PAID-CONTENT"))
	})
	for _, target := range []string{"/PAID", "/Paid", "/./paid", "/x/../paid", "/paid/.", "/%2e/paid"} {
		w := serveRaw(h, "GET", target)
		assert.Equal(t, 404, w.Code, target)
		assert.JSONEq(t, `{"error":"not found"}`, w.Body.String(), target)
	}
	assert.Equal(t, int32(0), reached.Load())
	assert.Equal(t, 402, serveRaw(h, "GET", "/paid").Code)
}

func TestPaidPostRouteIsNotMatchedByAnotherMethod(t *testing.T) {
	f := startMock(t)
	var reached atomic.Int32
	h := newGate(t, f, func(w http.ResponseWriter, _ *http.Request) { reached.Add(1) }, func(c *Config) {
		c.Routes = map[string]Route{"POST /submit": validRoute()}
	})
	assert.Equal(t, 402, serveRaw(h, "POST", "/submit").Code)
	assert.Equal(t, 200, serveRaw(h, "GET", "/submit").Code, "GET /submit is a different, unpaid route")
	assert.Equal(t, 200, serveRaw(h, "GET", "/other").Code)
	assert.Equal(t, 404, serveRaw(h, "POST", "/SUBMIT").Code)
}
