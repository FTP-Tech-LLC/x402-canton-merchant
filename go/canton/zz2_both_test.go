package canton

// Round-2 differential probes. The same file is copied into main (b7873d07) and
// into the diff; every test writes what it observed to a file named by an env
// variable, and the two files are compared byte for byte.

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func z2Targets() []string {
	return []string{
		// the previous round's set
		"/paid", "/paid/", "//paid", "/PAID", "/./paid", "/%70aid", "/paid%3Fx", "/PAID%3Fx",
		"/paid/x", "/free", "/api/Items", "/api/items", "/API/ITEMS/", "/paid?q=1", "/paid%2F", "/pa%2Fid",
		// escapes that survive net/http's one decode
		"/%2570aid", "/%252570aid", "/%25252570aid", "/%2525252570aid", "/%2550AID", "/%2570%2561%2569%2564",
		"/paid%252F", "/%2570aid/", "/%252E/paid", "/x/%252E%252E/paid", "/%252Fpaid", "/%2570aid?q=1",
		"/pa%2525id", "/%2570aid%25", "/%2570aid%253Fx", "/%2570aid%3Fx", "/paid%2500", "/PAID%2500",
		"/api/%2549tems", "/%2541PI/items", "/api%252Fitems", "/%2561pi/%2569tems/", "/%25%37%30aid",
		"/%2570aid%252F..%252Ffree", "/free%252F..%252Fpaid", "/paid%3F%252F..%252Ffree", "/paid%3F/../api/Items",
		"/paid%3F%252F..%252Fapi%252FItems", "/%2570aid%2523x", "/%2570aid#frag",
	}
}

type z2transcript struct{ lines []string }

func (tr *z2transcript) add(format string, args ...any) {
	tr.lines = append(tr.lines, fmt.Sprintf(format, args...))
}

func (tr *z2transcript) record(label string, w *httptest.ResponseRecorder, f *mockFacilitator, hits *atomic.Int32) {
	names := make([]string, 0, len(w.Header()))
	for name := range w.Header() {
		names = append(names, name)
	}
	sort.Strings(names)
	var hdr []string
	for _, name := range names {
		for _, value := range w.Header()[name] {
			if name == headerPaymentRequired || name == headerPaymentResponse {
				raw, _ := base64.StdEncoding.DecodeString(value)
				var decoded any
				_ = json.Unmarshal(raw, &decoded)
				canon, _ := json.Marshal(decoded)
				value = string(canon)
			}
			hdr = append(hdr, name+"="+value)
		}
	}
	tr.add("%s | %d | %s | %q | hits=%d verify=%d settle=%d", label, w.Code, strings.Join(hdr, "; "), w.Body.String(),
		hits.Load(), f.verifyCalls.Load(), f.settleCalls.Load())
}

func z2Routes(c *Config) {
	c.Routes = map[string]Route{
		"GET /paid":       {PayTo: testPayTo, Price: CC("0.01"), Memo: "order-1", Description: "paid data", MimeType: "application/json", ExecuteBeforeSeconds: 600},
		"POST /api/Items": {PayTo: testPayTo, Price: CC("0.02"), ExecuteBeforeSeconds: 30},
		"GET /free2":      {PayTo: testPayTo, Price: CC("0.03"), ExecuteBeforeSeconds: 5},
	}
}

func TestZ2Transcript(t *testing.T) {
	out := os.Getenv("Z2_TRANSCRIPT")
	if out == "" {
		t.Skip("Z2_TRANSCRIPT not set")
	}
	var tr z2transcript
	type step struct {
		method, target string
		pay            bool
		status         int
	}
	scenario := func(name string, script func(f *mockFacilitator), steps []step) {
		f := startMock(t)
		if script != nil {
			script(f)
		}
		var hits atomic.Int32
		status := 200
		h := newGate(t, f, func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Handler", "kept")
			w.Header().Set("X-Seen-Path", r.URL.Path)
			w.Header().Set("Cache-Control", "max-age=60")
			switch status {
			case 302:
				http.Redirect(w, r, "/elsewhere", http.StatusFound)
				return
			case 204:
				w.WriteHeader(204)
				return
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"data":42}`))
		}, z2Routes)
		var sig string
		for i, s := range steps {
			status = s.status
			if status == 0 {
				status = 200
			}
			signature := ""
			if s.pay {
				if sig == "" {
					sig = signatureFor(t, h, testPreparedTx)
				}
				signature = sig
			}
			w := do(h, s.method, s.target, signature)
			tr.record(fmt.Sprintf("%s#%d %s %s pay=%v", name, i, s.method, s.target, s.pay), w, f, &hits)
		}
	}
	var spellings []step
	for _, m := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "get"} {
		for _, target := range z2Targets() {
			spellings = append(spellings, step{method: m, target: target})
		}
	}
	scenario("unpaid-spellings", nil, spellings)
	// A payment header on every spelling: what the gate does with it.
	var paidSpellings []step
	for _, target := range z2Targets() {
		paidSpellings = append(paidSpellings, step{method: "GET", target: target, pay: true})
	}
	scenario("paid-spellings", nil, paidSpellings)
	scenario("paid-ok-then-replay", nil, []step{{"GET", "/paid", true, 0}, {"GET", "/paid", true, 0}, {"GET", "/PAID", true, 0}, {"HEAD", "/paid", true, 0}, {"GET", "/%2570aid", true, 0}})
	scenario("handler-500", nil, []step{{"GET", "/paid", true, 500}, {"GET", "/paid", true, 200}})
	scenario("handler-404", nil, []step{{"GET", "/paid", true, 404}})
	scenario("handler-302", nil, []step{{"GET", "/paid", true, 302}})
	scenario("handler-204", nil, []step{{"GET", "/paid", true, 204}, {"GET", "/paid", true, 204}})
	scenario("verify-invalid", func(f *mockFacilitator) {
		f.verify = func(_ int32, w http.ResponseWriter) { verifyInvalid(w, "invalid_exact_canton_payload") }
	}, []step{{"GET", "/paid", true, 0}})
	scenario("verify-down", func(f *mockFacilitator) {
		f.verify = func(_ int32, w http.ResponseWriter) { reply(w, 500, map[string]any{"error": "x"}) }
	}, []step{{"GET", "/paid", true, 0}})
	scenario("settle-pending-then-ok", func(f *mockFacilitator) {
		f.settle = func(n int32, w http.ResponseWriter) {
			if n <= 3 {
				settlePending(w, "")
				return
			}
			settleOK(w)
		}
	}, []step{{"GET", "/paid", true, 0}, {"GET", "/paid", true, 0}, {"GET", "/paid", true, 0}})
	scenario("already-settled", func(f *mockFacilitator) {
		f.settle = func(_ int32, w http.ResponseWriter) { settleAlreadySettled(w, testUpdateID) }
	}, []step{{"GET", "/paid", true, 0}, {"GET", "/paid", true, 0}})
	scenario("settle-refused", func(f *mockFacilitator) {
		f.settle = func(_ int32, w http.ResponseWriter) { settleRefused(w, "invalid_exact_canton_payment_expired") }
	}, []step{{"GET", "/paid", true, 0}})
	{
		f := startMock(t)
		var hits atomic.Int32
		ctx, cancel := context.WithCancel(context.Background())
		h := newGate(t, f, func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			cancel()
			time.Sleep(5 * time.Millisecond)
			_, _ = w.Write([]byte("x"))
		}, z2Routes)
		sig := signatureFor(t, h, testPreparedTx)
		w := doCtx(ctx, h, "GET", "/paid", sig)
		tr.record("payer-left GET /paid", w, f, &hits)
	}
	require.NoError(t, os.WriteFile(out, []byte(strings.Join(tr.lines, "\n")+"\n"), 0o644))
	t.Logf("%d transcript lines", len(tr.lines))
}

// The payment lifetime the gate gives each request (its lock TTL and write
// deadline), for every target and method.
func TestZ2Budgets(t *testing.T) {
	out := os.Getenv("Z2_BUDGETS")
	if out == "" {
		t.Skip("Z2_BUDGETS not set")
	}
	f := startMock(t)
	cfg := Config{FacilitatorURL: f.URL, SupportedWait: 2 * time.Second, Logger: slog.New(slog.DiscardHandler)}
	z2Routes(&cfg)
	g, err := buildGate(context.Background(), cfg)
	require.NoError(t, err)
	var lines []string
	for _, m := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		for _, target := range z2Targets() {
			r := httptest.NewRequest(m, target, nil)
			charged := g.httpServer.RequiresPayment(g.requestContext(r))
			lines = append(lines, fmt.Sprintf("%s %s charged=%v lifetime=%s", m, target, charged, g.routes.executeBefore(r)))
		}
	}
	require.NoError(t, os.WriteFile(out, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
}

// rawGet sends one request line over a real TCP connection and returns the
// status line and body.
func rawGet(t *testing.T, addr, method, target string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	require.NoError(t, err)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: shop.example\r\nConnection: close\r\n\r\n", method, target)
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return "read error: " + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return fmt.Sprintf("%d %q", resp.StatusCode, string(body))
}

// Real requests over the wire into a plain net/http server whose mux has the
// paid route and free routes next to it.
func TestZ2WireFreeRoutes(t *testing.T) {
	out := os.Getenv("Z2_WIRE")
	if out == "" {
		t.Skip("Z2_WIRE not set")
	}
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "%70aid"), []byte("FREE FILE NAMED %70aid"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "paid"), []byte("PAID FILE"), 0o600))
	f := startMock(t)
	var paidHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /paid", func(w http.ResponseWriter, _ *http.Request) {
		paidHits.Add(1)
		_, _ = w.Write([]byte("PAID CONTENT"))
	})
	mux.HandleFunc("GET /u/{name}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("free profile " + r.PathValue("name")))
	})
	mux.Handle("GET /files/", http.StripPrefix("/files", http.FileServer(http.Dir(dir))))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("free catch-all " + r.URL.Path))
	})
	h := newGate(t, f, mux.ServeHTTP, func(c *Config) {
		c.Routes = map[string]Route{
			"GET /paid":       {PayTo: testPayTo, Price: CC("0.01")},
			"GET /u/alice":    {PayTo: testPayTo, Price: CC("0.01")},
			"GET /files/paid": {PayTo: testPayTo, Price: CC("0.01")},
		}
	})
	server := httptest.NewServer(h)
	defer server.Close()
	addr := strings.TrimPrefix(server.URL, "http://")
	var lines []string
	for _, target := range []string{
		"/paid", "/%70aid", "/%2570aid", "/%252570aid", "/PAID", "/%2550AID",
		"/u/alice", "/u/%61lice", "/u/%2561lice", "/u/%252561lice", "/u/ALICE", "/u/%2541LICE",
		"/files/paid", "/files/%70aid", "/files/%2570aid", "/files/free", "/x/%252E%252E/paid", "/free%252F..%252Fpaid",
	} {
		muxPattern := ""
		if r, err := http.NewRequest("GET", server.URL+target, nil); err == nil {
			_, muxPattern = mux.Handler(r)
		}
		lines = append(lines, fmt.Sprintf("GET %s mux=%q -> %s", target, muxPattern, rawGet(t, addr, "GET", target)))
	}
	lines = append(lines, fmt.Sprintf("paid handler hits=%d verify=%d settle=%d", paidHits.Load(), f.verifyCalls.Load(), f.settleCalls.Load()))
	require.NoError(t, os.WriteFile(out, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
}

// A client cannot make its request synthetic: whatever it sends, the plain
// net/http gate behind a blind wrapper warns at its first 402.
func TestZ2ClientCannotSuppressTheWarning(t *testing.T) {
	for _, name := range []string{"X-Synthetic", "Synthetic", "X-Paidpath-Synthetic", "X-Canton-Check", "X-Probe"} {
		t.Run(name, func(t *testing.T) {
			blindFlushWarned.Store(false)
			t.Cleanup(func() { blindFlushWarned.Store(false) })
			f := startMock(t)
			logger, logs := capturingLogger()
			var hits atomic.Int32
			h := newGate(t, f, okHandler(&hits), func(c *Config) { c.Logger = logger })
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(blindWriter{w}, r) }))
			defer server.Close()
			req, _ := http.NewRequest("GET", server.URL+"/paid?synthetic=1&probe=1", nil)
			req.Header.Set(name, "1")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			_ = resp.Body.Close()
			require.Equal(t, 402, resp.StatusCode)
			require.True(t, strings.Contains(logs.String(), "cannot see the connection"), "a real request is never synthetic")
		})
	}
}
