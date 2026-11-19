package canton

// Final probes that compile on main (b7873d07) and on the diff: run in both
// trees and compare.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// blindWriter is a ResponseWriter wrapper in front of the gate without Unwrap:
// the gate cannot see the connection through it.
type blindWriter struct{ http.ResponseWriter }

// The plain net/http gate behind a blind wrapper. Every payment is preceded by
// the unpaid 402 its client asked for: the operator should hear about the blind
// wrapper by then, before any money moves.
func TestFinalBlindWrapperIsReportedBeforeTheFirstPayment(t *testing.T) {
	blindFlushWarned.Store(false)
	t.Cleanup(func() { blindFlushWarned.Store(false) })
	f := startMock(t)
	logger, logs := capturingLogger()
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits), func(c *Config) { c.Logger = logger })
	blind := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(blindWriter{w}, r) })
	server := httptest.NewServer(blind)
	defer server.Close()

	resp, err := http.Get(server.URL + "/paid")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, 402, resp.StatusCode)
	warnedAtOffer := strings.Contains(logs.String(), "cannot see the connection")

	// The client pays: the first payment goes through the blind wrapper.
	unpaid := httptest.NewRecorder()
	unpaid.Header().Set(headerPaymentRequired, resp.Header.Get(headerPaymentRequired))
	sig := rebuildSignature(unpaid, testPreparedTx)
	req, _ := http.NewRequest("GET", server.URL+"/paid", nil)
	req.Header.Set(headerPaymentSig, sig)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	warnedAfterPayment := strings.Contains(logs.String(), "cannot see the connection")
	t.Logf("warned at the first 402 (before any payment): %v; warned after the first paid delivery: %v; settle calls: %d",
		warnedAtOffer, warnedAfterPayment, f.settleCalls.Load())
	require.True(t, warnedAfterPayment, "a claimed delivery through a blind wrapper is warned about")
	require.True(t, warnedAtOffer, "the blind wrapper must be reported at the first 402, before the first payment moves money")
}

// transcript drives the same requests through a fresh gate and records every
// observable: status, headers, body, handler runs, facilitator calls.
type transcript struct{ lines []string }

func (tr *transcript) add(format string, args ...any) {
	tr.lines = append(tr.lines, fmt.Sprintf(format, args...))
}

func (tr *transcript) record(label string, w *httptest.ResponseRecorder, f *mockFacilitator, hits *atomic.Int32) {
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

func TestFinalTranscript(t *testing.T) {
	out := os.Getenv("FINAL_TRANSCRIPT")
	if out == "" {
		t.Skip("FINAL_TRANSCRIPT not set")
	}
	var tr transcript
	routes := func(c *Config) {
		c.Routes = map[string]Route{
			"GET /paid":       {PayTo: testPayTo, Price: CC("0.01"), Memo: "order-1", Description: "paid data", MimeType: "application/json"},
			"POST /api/Items": {PayTo: testPayTo, Price: CC("0.02")},
		}
	}
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
		}, routes)
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
		for _, target := range []string{"/paid", "/paid/", "//paid", "/PAID", "/./paid", "/%70aid", "/paid%3Fx", "/PAID%3Fx",
			"/paid/x", "/free", "/api/Items", "/api/items", "/API/ITEMS/", "/paid?q=1", "/paid%2F", "/pa%2Fid"} {
			spellings = append(spellings, step{method: m, target: target})
		}
	}
	scenario("unpaid-spellings", nil, spellings)
	scenario("paid-ok-then-replay", nil, []step{{"GET", "/paid", true, 0}, {"GET", "/paid", true, 0}, {"GET", "/PAID", true, 0}, {"HEAD", "/paid", true, 0}})
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
	scenario("payer-left", nil, nil)
	// A payer who left before settle.
	{
		f := startMock(t)
		var hits atomic.Int32
		h := newGate(t, f, okHandler(&hits), routes)
		sig := signatureFor(t, h, testPreparedTx)
		ctx, cancel := context.WithCancel(context.Background())
		hh := newGate(t, f, func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			cancel()
			time.Sleep(5 * time.Millisecond)
			_, _ = w.Write([]byte("x"))
		}, routes)
		w := doCtx(ctx, hh, "GET", "/paid", sig)
		tr.record("payer-left GET /paid", w, f, &hits)
		_ = h
	}
	require.NoError(t, os.WriteFile(out, []byte(strings.Join(tr.lines, "\n")+"\n"), 0o644))
	t.Logf("%d transcript lines", len(tr.lines))
}

// deadlineWriter is a blind wrapper that does forward SetWriteDeadline (so the
// write-deadline warning cannot stand in for the blind-flush one).
type deadlineWriter struct{ http.ResponseWriter }

func (d deadlineWriter) SetWriteDeadline(t time.Time) error {
	return http.NewResponseController(d.ResponseWriter).SetWriteDeadline(t)
}

func TestFinalBlindWrapperWarningOrder(t *testing.T) {
	for _, wrap := range []struct {
		name string
		fn   func(http.ResponseWriter) http.ResponseWriter
	}{
		{"no-unwrap-no-deadline", func(w http.ResponseWriter) http.ResponseWriter { return blindWriter{w} }},
		{"no-unwrap-with-deadline", func(w http.ResponseWriter) http.ResponseWriter { return deadlineWriter{w} }},
	} {
		t.Run(wrap.name, func(t *testing.T) {
			blindFlushWarned.Store(false)
			writeDeadlineWarned.Store(false)
			t.Cleanup(func() { blindFlushWarned.Store(false); writeDeadlineWarned.Store(false) })
			f := startMock(t)
			logger, logs := capturingLogger()
			var hits atomic.Int32
			h := newGate(t, f, okHandler(&hits), func(c *Config) { c.Logger = logger })
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(wrap.fn(w), r) }))
			defer server.Close()
			resp, err := http.Get(server.URL + "/paid")
			require.NoError(t, err)
			_ = resp.Body.Close()
			atOffer := logs.String()
			unpaid := httptest.NewRecorder()
			unpaid.Header().Set(headerPaymentRequired, resp.Header.Get(headerPaymentRequired))
			req, _ := http.NewRequest("GET", server.URL+"/paid", nil)
			req.Header.Set(headerPaymentSig, rebuildSignature(unpaid, testPreparedTx))
			resp, err = http.DefaultClient.Do(req)
			require.NoError(t, err)
			_ = resp.Body.Close()
			require.Equal(t, 200, resp.StatusCode)
			all := logs.String()
			t.Logf("%s: at the 402: blindFlush=%v writeDeadline=%v | after the paid 200 (settle=%d): blindFlush=%v writeDeadline=%v",
				wrap.name, strings.Contains(atOffer, "cannot see the connection"), strings.Contains(atOffer, "cannot lift the write deadline"),
				f.settleCalls.Load(), strings.Contains(all, "cannot see the connection"), strings.Contains(all, "cannot lift the write deadline"))
		})
	}
}
