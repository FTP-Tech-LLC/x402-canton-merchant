package canton

// Final diff-scoped probes for the probe fast path (gate.wrap + gatedMethods).
// Lives only in the audit copy of the new tree.

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/paidpath"
)

// forgedProbeCtx answers ctx.Value(paidpath.probeKey{}) with a probe. Only Go
// code in the process that builds the request context can do this; an HTTP
// client cannot.
type forgedProbeCtx struct {
	context.Context
	probe *paidpath.Probe
}

func (c forgedProbeCtx) Value(key any) any {
	if reflect.TypeOf(key).String() == "paidpath.probeKey" {
		return c.probe
	}
	return c.Context.Value(key)
}

func sweepRoutes() map[string]Route {
	r := func() Route { return Route{PayTo: testPayTo, Price: CC("0.01")} }
	return map[string]Route{
		"GET /paid":               r(),
		"POST /api/v1/Items":      r(),
		"PUT /a.b/c+d":            r(),
		"DELETE /x/y;z":           r(),
		"PATCH /caf~e/@me":        r(),
		"GET /x/y":                r(),
		"POST /x/y":               r(),
		"GET /deep/a/b/c":         r(),
		"GET /q=1,2!(x)&y":        r(),
		"GET /UPPER/Case":         r(),
		"DELETE /d-1/e_2/f.3/g~4": r(),
	}
}

func buildSweepGate(t *testing.T, routes map[string]Route) *gate {
	t.Helper()
	f := startMock(t)
	g, err := buildGate(context.Background(), Config{FacilitatorURL: f.URL, Routes: routes, SupportedWait: 2 * time.Second, Logger: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)
	return g
}

func swapCase(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
			b.WriteRune(c - 32)
		case c >= 'A' && c <= 'Z':
			b.WriteRune(c + 32)
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

func spellingsOf(p string) []string {
	out := []string{
		p, p + "/", "/" + p, p + "//", strings.ToUpper(p), strings.ToLower(p), swapCase(p),
		"/." + p, p + "/.", "/x/.." + p, p + "/x/..", p + "/..", "/%2E" + p, p + "/%2e", "/%2e%2e" + p,
		p + "%3F", p + "%3Fx", p + "%3f/x", p + "%23", p + "%23x", strings.ToUpper(p) + "%3Fx", swapCase(p) + "%23",
		"/." + p + "%3Fx", p + "/%3Fx", p + "/.%3Fx", p + "/%2e%3F", p + "//%3F", "/" + p + "%3F",
		p + "?q=1", p + "?", p + "#frag", p + "%3F#x", p + ";x", p + "%00", p + "%0A", p + "%20", p + "%09",
		p + "%2F", p + "%5C", p + "\\", "http://h" + p, "http://h" + strings.ToUpper(p) + "%3Fx",
		strings.ToUpper(p) + "/", "//" + strings.ToLower(p) + "/", p + "%3F%3F", p + "%3Fa?b#c",
	}
	// One character percent-encoded at a time, upper and lower hex.
	for i := 1; i < len(p); i++ {
		c := p[i]
		if c == '/' {
			out = append(out, p[:i]+"%2F"+p[i+1:], p[:i]+"%2f"+p[i+1:], p[:i]+"%5C"+p[i+1:], p[:i]+"\\"+p[i+1:])
			continue
		}
		out = append(out, p[:i]+fmt.Sprintf("%%%02X", c)+p[i+1:], p[:i]+fmt.Sprintf("%%%02x", c)+p[i+1:])
	}
	// Everything but slashes encoded.
	var full strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			full.WriteByte('/')
		} else {
			fmt.Fprintf(&full, "%%%02X", p[i])
		}
	}
	out = append(out, full.String(), full.String()+"%3Fz", strings.ToLower(full.String()))
	return out
}

func parseRaw(method, target string) (*http.Request, error) {
	raw := fmt.Sprintf("%s %s HTTP/1.1\r\nHost: h\r\n\r\n", method, target)
	return http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
}

// coreClass runs a real unpaid request through the gate and says what it did.
func coreClass(g *gate, r *http.Request) string {
	var ran atomic.Bool
	h := g.wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { ran.Store(true); w.WriteHeader(299) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	switch {
	case ran.Load():
		return "pass"
	case rec.Code == http.StatusPaymentRequired && rec.Header().Get(headerPaymentRequired) != "":
		return "charge"
	case rec.Code == http.StatusNotFound:
		return "refuse"
	default:
		return fmt.Sprintf("other-%d", rec.Code)
	}
}

func probeAnswer(t *testing.T, g *gate, r *http.Request) map[string]bool {
	t.Helper()
	p := &paidpath.Probe{}
	pr := r.WithContext(forgedProbeCtx{Context: r.Context(), probe: p}) //nolint:contextcheck // forges the probe context on purpose
	pr.Header = http.Header{}
	pr.Body = http.NoBody
	rec := httptest.NewRecorder()
	// As paidpath.ask does: a probe reaches next (a gate composed inside adds its
	// keys), and next is a no-op.
	g.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, pr)
	require.True(t, p.Answered, "the canton gate answers a probe")
	require.Zero(t, rec.Body.Len(), "a probe writes nothing")
	require.Empty(t, rec.Header(), "a probe sets no header")
	return p.Methods
}

func TestFinalProbeSweepMatchesCore(t *testing.T) {
	g := buildSweepGate(t, sweepRoutes())
	keyPaths := map[string]bool{}
	for key := range sweepRoutes() {
		_, p, _ := strings.Cut(key, " ")
		keyPaths[p] = true
	}
	methods := []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "get", "Post"}
	var checked, parseFailed, gatedCases int
	var overApprox []string
	var falseNeg []string
	seen := map[string]bool{}
	for p := range keyPaths {
		for _, target := range spellingsOf(p) {
			if seen[target] {
				continue
			}
			seen[target] = true
			r0, err := parseRaw("GET", target)
			if err != nil {
				parseFailed++
				continue
			}
			probe := probeAnswer(t, g, r0)
			// The probe is method-agnostic: same answer for every method of the request.
			for _, m := range methods {
				rm, err := parseRaw(m, target)
				if err != nil {
					continue
				}
				require.Equal(t, probe, probeAnswer(t, g, rm), "probe answer depends on the request method for %s %q", m, target)
			}
			for _, m := range methods {
				r, err := parseRaw(m, target)
				if err != nil {
					continue
				}
				class := coreClass(g, r)
				checked++
				key := strings.ToUpper(m)
				if key == http.MethodHead {
					key = http.MethodGet
				}
				coreGated := class == "charge" || class == "refuse"
				if strings.HasPrefix(class, "other") {
					t.Errorf("unexpected core answer %s for %s %q", class, m, target)
				}
				if coreGated {
					gatedCases++
					if !probe[key] {
						falseNeg = append(falseNeg, fmt.Sprintf("%s %q path=%q raw=%q core=%s probe=%v", m, target, r.URL.Path, r.URL.RawPath, class, probe))
					}
				}
				// Slow path (paidpath.Gates) must equal the core for key methods.
				if paidpath.Gates(g.wrap, r, key) != func() bool {
					rk, _ := parseRaw(key, target)
					c := coreClass(g, rk)
					return c == "charge" || c == "refuse"
				}() {
					t.Errorf("slow path disagrees with core for %s %q", key, target)
				}
				if m == key && probe[key] && !coreGated {
					overApprox = append(overApprox, fmt.Sprintf("%s %q path=%q core=%s", m, target, r.URL.Path, class))
					if !strings.ContainsAny(r.URL.Path, "?#") {
						t.Errorf("over-approximation without ?/# in the decoded path: %s %q path=%q", m, target, r.URL.Path)
					}
				}
			}
		}
	}
	sort.Strings(overApprox)
	t.Logf("spellings=%d parseFailed=%d requests=%d coreGated=%d falseNegatives=%d overApprox=%d", len(seen), parseFailed, checked, gatedCases, len(falseNeg), len(overApprox))
	for _, line := range overApprox {
		t.Logf("over-approx (probe gated, core passes): %s", line)
	}
	for _, line := range falseNeg {
		t.Errorf("FALSE NEGATIVE (core gates, probe does not): %s", line)
	}
}

// A probe that carries a real, valid payment for a paid route moves nothing:
// no /verify, no /settle, no lock, no record, nothing written.
func TestFinalProbeWithPaymentMovesNothing(t *testing.T) {
	f := startMock(t)
	store := NewMemoryStore()
	g, err := buildGate(context.Background(), Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: 2 * time.Second, Logger: nopLogger(), Store: store})
	require.NoError(t, err)
	var hits atomic.Int32
	h := g.wrap(okHandler(&hits))
	probeHandler := g.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})) // paidpath.ask's next
	sig := signatureFor(t, h, testPreparedTx)
	for _, m := range []string{"GET", "HEAD", "POST"} {
		p := &paidpath.Probe{}
		r := httptest.NewRequest(m, "/paid", strings.NewReader("body"))
		r.Header.Set(headerPaymentSig, sig)
		r = r.WithContext(forgedProbeCtx{Context: r.Context(), probe: p})
		rec := httptest.NewRecorder()
		probeHandler.ServeHTTP(rec, r)
		require.True(t, p.Answered)
		require.Equal(t, map[string]bool{"GET": true}, p.Methods)
		require.Zero(t, rec.Body.Len())
		require.Empty(t, rec.Header())
	}
	require.Zero(t, f.verifyCalls.Load())
	require.Zero(t, f.settleCalls.Load())
	require.Zero(t, hits.Load())
	// The same payment, sent for real afterwards, is delivered once.
	w := do(h, "GET", "/paid", sig)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Equal(t, int32(1), f.settleCalls.Load())
	w = do(h, "GET", "/paid", sig)
	require.Equal(t, 402, w.Code, w.Body.String())
	require.Equal(t, int32(1), hits.Load())
}

// Anything an HTTP client can send (headers, query, odd methods) is a real
// request: it gets the offer, never the probe path.
func TestFinalClientCannotForgeAProbe(t *testing.T) {
	f := startMock(t)
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits))
	server := httptest.NewServer(h)
	defer server.Close()
	headers := []string{"X-Probe", "Probe", "Paidpath-Probe", "X-Paidpath-Probe", "X-Canton-Probe", "Answered", "Methods", "Canton-Probe", "X-Internal-Probe"}
	for _, name := range headers {
		req, _ := http.NewRequest("GET", server.URL+"/paid?probe=1&answered=true", nil)
		req.Header.Set(name, "1")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, 402, resp.StatusCode, name)
		require.NotEmpty(t, resp.Header.Get(headerPaymentRequired), name)
	}
	// A context value under another package's key of the same shape is not a probe.
	type probeKey struct{}
	r := httptest.NewRequest("GET", "/paid", nil)
	r = r.WithContext(context.WithValue(r.Context(), probeKey{}, &paidpath.Probe{}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	require.Equal(t, 402, w.Code)
	require.Zero(t, hits.Load())
}

// The probe is answered by the first canton gate only: a second canton gate
// composed inside the same function is not asked (README: compose nothing in).
func TestFinalComposedGatesFastVersusSlow(t *testing.T) {
	gA := buildSweepGate(t, map[string]Route{"GET /a": {PayTo: testPayTo, Price: CC("0.01")}})
	gB := buildSweepGate(t, map[string]Route{"GET /b": {PayTo: testPayTo, Price: CC("0.01")}})
	composed := func(h http.Handler) http.Handler { return gA.wrap(gB.wrap(h)) }
	r := httptest.NewRequest("GET", "/B", nil) // a loose spelling of B's key, routed by no framework route
	fast := paidpath.AnyMethodGated(composed, r)
	slow := paidpath.Gates(composed, r, "GET")
	t.Logf("composed A(B): fast AnyMethodGated=%v slow Gates(GET)=%v", fast, slow)
}
