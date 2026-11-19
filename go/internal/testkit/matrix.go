package testkit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
)

// Ctx is what a paid handler script can do, in framework-neutral terms. Each
// adapter's tests implement it over their own context type.
type Ctx interface {
	Request() *http.Request
	SetHeader(key, value string)
	// Respond answers with a status and a body, the framework's normal way.
	Respond(status int, body string)
	Redirect(status int, location string)
	// Fail ends the handler with the framework's native failure for a status: a
	// returned echo.NewHTTPError, a gin AbortWithStatus.
	Fail(status int)
	Panic()
	// RespondNoContentWithBody writes a 204 and then a body.
	RespondNoContentWithBody()
}

// Build builds the framework engine: the recovery middleware outermost, the
// gate middleware inside it, GET /paid running the script, GET /free answering
// "free", and POST-less. mw is gate.Middleware().
type Build func(t *testing.T, mw func(http.Handler) http.Handler, script func(Ctx)) http.Handler

// Script helpers.

type probe struct {
	hits  atomic.Int32
	payer atomic.Value
	retry atomic.Bool
}

func (p *probe) ok(c Ctx) {
	p.hits.Add(1)
	payer, _ := canton.PayerFrom(c.Request().Context())
	p.payer.Store(payer)
	p.retry.Store(canton.IsRetry(c.Request().Context()))
	c.SetHeader("Set-Cookie", "session=paid")
	c.SetHeader("X-Handler", "kept")
	c.Respond(200, `{"data":42}`)
}

// RunMatrix runs the money matrix through one framework.
func RunMatrix(t *testing.T, build Build) {
	setup := func(t *testing.T, script func(Ctx), mutate ...func(*canton.Config)) (*Facilitator, http.Handler) {
		f := StartFacilitator(t)
		g := NewGate(t, f, mutate...)
		return f, build(t, g.Middleware(), script)
	}
	pay := func(t *testing.T, h http.Handler) string { return SignatureFor(t, h, PreparedTx) }

	t.Run("a fresh payment is delivered once with its receipt", func(t *testing.T) {
		var p probe
		f, h := setup(t, p.ok)
		w := Do(h, "GET", "/paid", pay(t, h))
		require.Equal(t, 200, w.Code, w.Body.String())
		assert.JSONEq(t, `{"data":42}`, w.Body.String())
		receipt := PaymentResponse(t, w)
		assert.Equal(t, true, receipt["success"])
		assert.Equal(t, UpdateID, receipt["transaction"])
		assert.Contains(t, w.Header().Get("Cache-Control"), "private")
		assert.Equal(t, "kept", w.Header().Get("X-Handler"))
		assert.Equal(t, "session=paid", w.Header().Get("Set-Cookie"))
		assert.Equal(t, Payer, p.payer.Load(), "PayerFrom works inside the framework handler")
		assert.False(t, p.retry.Load())
		assert.Equal(t, int32(1), f.SettleCalls.Load())
	})

	t.Run("a replay is refused as redeemed and the handler does not run", func(t *testing.T) {
		var p probe
		_, h := setup(t, p.ok)
		sig := pay(t, h)
		require.Equal(t, 200, Do(h, "GET", "/paid", sig).Code)
		w := Do(h, "GET", "/paid", sig)
		assert.Equal(t, 402, w.Code)
		assert.JSONEq(t, `{"error":"payment_already_redeemed"}`, w.Body.String())
		assert.Equal(t, int32(1), p.hits.Load())
	})

	t.Run("already_settled is delivered once", func(t *testing.T) {
		var p probe
		f, h := setup(t, p.ok)
		f.OnSettle(func(_ int32, w http.ResponseWriter) { SettleAlreadySettled(w) })
		sig := pay(t, h)
		w := Do(h, "GET", "/paid", sig)
		assert.Equal(t, 200, w.Code)
		assert.Equal(t, UpdateID, PaymentResponse(t, w)["transaction"])
		assert.Equal(t, 402, Do(h, "GET", "/paid", sig).Code)
		assert.Equal(t, int32(1), p.hits.Load())
	})

	t.Run("pending then success is delivered once", func(t *testing.T) {
		var p probe
		f, h := setup(t, p.ok)
		f.OnSettle(func(n int32, w http.ResponseWriter) {
			if n == 1 {
				SettlePending(w)
				return
			}
			SettleOK(w)
		})
		assert.Equal(t, 200, Do(h, "GET", "/paid", pay(t, h)).Code)
		assert.Equal(t, int32(1), p.hits.Load())
		assert.Equal(t, int32(2), f.SettleCalls.Load())
	})

	t.Run("an unknown outcome is 503 do NOT pay again and the retry is served once", func(t *testing.T) {
		var p probe
		f, h := setup(t, p.ok, func(c *canton.Config) { c.SettleCalls = 1 })
		f.OnSettle(func(n int32, w http.ResponseWriter) {
			if n == 1 {
				w.WriteHeader(502)
				return
			}
			SettleAlreadySettled(w)
		})
		sig := pay(t, h)
		first := Do(h, "GET", "/paid", sig)
		assert.Equal(t, 503, first.Code)
		assert.Contains(t, first.Body.String(), "do NOT pay again")
		assert.NotContains(t, first.Body.String(), "data")
		assert.Equal(t, "settlement_pending", PaymentResponse(t, first)["errorReason"])
		assert.Empty(t, first.Header().Get("Set-Cookie"), "the handler's headers never reach a gate answer")
		assert.Equal(t, 200, Do(h, "GET", "/paid", sig).Code)
		assert.Equal(t, 402, Do(h, "GET", "/paid", sig).Code)
	})

	t.Run("a /verify outage is 503 and nothing runs", func(t *testing.T) {
		var p probe
		f, h := setup(t, p.ok)
		sig := pay(t, h)
		f.OnVerify(func(_ int32, w http.ResponseWriter) { Reply(w, 503, map[string]string{"error": "scan_busy"}) })
		w := Do(h, "GET", "/paid", sig)
		assert.Equal(t, 503, w.Code)
		assert.Contains(t, w.Body.String(), "verification unavailable")
		assert.Zero(t, p.hits.Load())
		assert.Zero(t, f.SettleCalls.Load())
	})

	t.Run("an invalid payment is 402 and nothing runs", func(t *testing.T) {
		var p probe
		f, h := setup(t, p.ok)
		sig := pay(t, h)
		f.OnVerify(func(_ int32, w http.ResponseWriter) { VerifyInvalid(w, "invalid_exact_canton_expired") })
		assert.Equal(t, 402, Do(h, "GET", "/paid", sig).Code)
		assert.Zero(t, p.hits.Load())
	})

	t.Run("handler 4xx and 5xx pass through unchanged and are not settled", func(t *testing.T) {
		for _, status := range []int{400, 404, 429, 500, 503} {
			f, h := setup(t, func(c Ctx) {
				c.SetHeader("X-Handler", "kept")
				c.Respond(status, "handler says no")
			})
			w := Do(h, "GET", "/paid", pay(t, h))
			assert.Equal(t, status, w.Code)
			assert.Equal(t, "handler says no", w.Body.String())
			assert.Equal(t, "kept", w.Header().Get("X-Handler"))
			assert.Zero(t, f.SettleCalls.Load(), "status %d", status)
		}
	})

	t.Run("a native handler failure is not settled", func(t *testing.T) {
		for _, status := range []int{401, 404, 500} {
			f, h := setup(t, func(c Ctx) { c.Fail(status) })
			w := Do(h, "GET", "/paid", pay(t, h))
			assert.Equal(t, status, w.Code)
			assert.Zero(t, f.SettleCalls.Load(), "status %d", status)
			assert.Empty(t, w.Header().Get("PAYMENT-RESPONSE"))
		}
	})

	t.Run("a redirect is 502 and not settled, unless SettleOnRedirect", func(t *testing.T) {
		script := func(c Ctx) { c.SetHeader("Set-Cookie", "a=b"); c.Redirect(302, "/elsewhere") }
		f, h := setup(t, script)
		w := Do(h, "GET", "/paid", pay(t, h))
		assert.Equal(t, 502, w.Code)
		assert.Contains(t, w.Body.String(), "nothing was charged")
		assert.Empty(t, w.Header().Get("Location"))
		assert.Empty(t, w.Header().Get("Set-Cookie"))
		assert.Zero(t, f.SettleCalls.Load())

		f2, h2 := setup(t, script, func(c *canton.Config) { c.SettleOnRedirect = true })
		w = Do(h2, "GET", "/paid", pay(t, h2))
		assert.Equal(t, 302, w.Code)
		assert.Equal(t, "/elsewhere", w.Header().Get("Location"))
		assert.Equal(t, int32(1), f2.SettleCalls.Load())
	})

	t.Run("a panic is not settled and releases the lock", func(t *testing.T) {
		var p probe
		var calls atomic.Int32
		f, h := setup(t, func(c Ctx) {
			if calls.Add(1) == 1 {
				c.Panic()
			}
			p.ok(c)
		})
		sig := pay(t, h)
		first := Do(h, "GET", "/paid", sig)
		assert.Equal(t, 500, first.Code)
		assert.Zero(t, f.SettleCalls.Load())
		assert.Equal(t, 200, Do(h, "GET", "/paid", sig).Code, "the same payment is not stuck behind a leaked lock")
	})

	t.Run("a payer who left during settle is served once on the retry", func(t *testing.T) {
		var p probe
		f, h := setup(t, p.ok)
		ctx, leave := context.WithCancel(context.Background())
		f.OnSettle(func(n int32, w http.ResponseWriter) {
			if n == 1 {
				leave()
				SettleOK(w)
				return
			}
			SettleAlreadySettled(w)
		})
		sig := pay(t, h)
		first := DoCtx(ctx, h, "GET", "/paid", sig)
		assert.Equal(t, 503, first.Code)
		assert.NotContains(t, first.Body.String(), "data")
		retry := Do(h, "GET", "/paid", sig)
		assert.Equal(t, 200, retry.Code)
		assert.True(t, p.retry.Load(), "IsRetry is true inside the framework handler")
		assert.Equal(t, 402, Do(h, "GET", "/paid", sig).Code)
	})

	t.Run("a handler refusal on the retry of a charged payment says do NOT pay again", func(t *testing.T) {
		var calls atomic.Int32
		var p probe
		f, h := setup(t, func(c Ctx) {
			if calls.Add(1) == 2 {
				c.Respond(429, "slow down")
				return
			}
			p.ok(c)
		})
		ctx, leave := context.WithCancel(context.Background())
		f.OnSettle(func(n int32, w http.ResponseWriter) {
			if n == 1 {
				leave()
				SettleOK(w)
				return
			}
			SettleAlreadySettled(w)
		})
		sig := pay(t, h)
		require.Equal(t, 503, DoCtx(ctx, h, "GET", "/paid", sig).Code)
		limited := Do(h, "GET", "/paid", sig)
		assert.Equal(t, 503, limited.Code)
		assert.Contains(t, limited.Body.String(), "do NOT pay again")
		assert.Equal(t, 200, Do(h, "GET", "/paid", sig).Code)
	})

	t.Run("duplicates in flight run one handler and one settle", func(t *testing.T) {
		var hits atomic.Int32
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		f, h := setup(t, func(c Ctx) {
			hits.Add(1)
			once.Do(func() { close(entered) })
			<-release
			c.Respond(200, `{"data":42}`)
		})
		sig := pay(t, h)
		first := make(chan int, 1)
		go func() { first <- Do(h, "GET", "/paid", sig).Code }()
		<-entered
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				w := Do(h, "GET", "/paid", sig)
				assert.Equal(t, 503, w.Code)
				assert.Contains(t, w.Body.String(), "payment in progress")
			}()
		}
		wg.Wait()
		close(release)
		assert.Equal(t, 200, <-first)
		assert.Equal(t, int32(1), hits.Load())
		assert.Equal(t, int32(1), f.SettleCalls.Load())
	})

	t.Run("two instances on one Redis deliver once", func(t *testing.T) {
		f := StartFacilitator(t)
		redis := NewFakeRedis()
		store := func(c *canton.Config) { c.Store = canton.NewRedisStore(redis, "x:") }
		var pa, pb probe
		a := build(t, NewGate(t, f, store).Middleware(), pa.ok)
		b := build(t, NewGate(t, f, store).Middleware(), pb.ok)
		sig := SignatureFor(t, a, PreparedTx)
		require.Equal(t, 200, Do(a, "GET", "/paid", sig).Code)
		w := Do(b, "GET", "/paid", sig)
		assert.Equal(t, 402, w.Code)
		assert.Zero(t, pb.hits.Load())
		assert.Equal(t, int32(1), f.SettleCalls.Load())
	})

	t.Run("a refusal answer carries none of the handler's headers", func(t *testing.T) {
		f, h := setup(t, func(c Ctx) {
			c.SetHeader("Set-Cookie", "access=paid")
			c.SetHeader("X-Internal", "secret")
			c.Respond(200, "paid-body")
		})
		f.OnSettle(func(_ int32, w http.ResponseWriter) { SettleRefused(w, "invalid_exact_canton_insufficient_balance") })
		w := Do(h, "GET", "/paid", pay(t, h))
		assert.Equal(t, 402, w.Code)
		assert.Empty(t, w.Header().Get("Set-Cookie"))
		assert.Empty(t, w.Header().Get("X-Internal"))
		assert.NotContains(t, w.Body.String(), "paid-body")
	})

	t.Run("204 with a body is delivered once", func(t *testing.T) {
		var calls atomic.Int32
		f, h := setup(t, func(c Ctx) { calls.Add(1); c.RespondNoContentWithBody() })
		sig := pay(t, h)
		w := Do(h, "GET", "/paid", sig)
		assert.Equal(t, 204, w.Code)
		assert.Empty(t, w.Body.String())
		assert.Equal(t, 402, Do(h, "GET", "/paid", sig).Code)
		assert.Equal(t, int32(1), calls.Load())
		assert.Equal(t, int32(1), f.SettleCalls.Load())
	})

	t.Run("free routes pass through untouched", func(t *testing.T) {
		var p probe
		f, h := setup(t, p.ok)
		w := Do(h, "GET", "/free", "")
		assert.Equal(t, 200, w.Code)
		assert.Equal(t, "free", w.Body.String())
		assert.Zero(t, f.VerifyCalls.Load())
	})

	t.Run("HEAD with a payment is refused and spends nothing", func(t *testing.T) {
		var p probe
		f, h := setup(t, p.ok)
		sig := pay(t, h)
		assert.Contains(t, []int{404, 405}, Do(h, "HEAD", "/paid", sig).Code, "refused by the gate (405) or unrouted by the framework (404)")
		assert.Zero(t, f.SettleCalls.Load())
	})

	t.Run("odd spellings of the paid path never serve paid content free", func(t *testing.T) {
		var p probe
		_, h := setup(t, p.ok)
		for _, target := range []string{"/paid/", "//paid", "/%70aid", "/PAID", "/./paid", "/paid?x=1"} {
			w := Do(h, "GET", target, "")
			assert.NotContains(t, w.Body.String(), "data", target)
			assert.Zero(t, p.hits.Load(), target)
		}
	})

	t.Run("an unpaid request gets the offer", func(t *testing.T) {
		var p probe
		_, h := setup(t, p.ok)
		w := Do(h, "GET", "/paid", "")
		assert.Equal(t, 402, w.Code)
		var required map[string]any
		DecodeHeader(t, w.Header().Get("PAYMENT-REQUIRED"), &required)
		accepted := required["accepts"].([]any)[0].(map[string]any)
		assert.Equal(t, "100000000", accepted["amount"])
		assert.Zero(t, p.hits.Load())
	})

	t.Run("a failed final flush gives the claim back and the retry is served once", func(t *testing.T) {
		var p probe
		f, h := setup(t, p.ok)
		f.OnSettle(func(n int32, w http.ResponseWriter) {
			if n == 1 {
				SettleOK(w)
				return
			}
			SettleAlreadySettled(w)
		})
		sig := pay(t, h)
		ServeThrough(h, BrokenFlushWriter{httptest.NewRecorder()}, "GET", "/paid", sig)
		retry := Do(h, "GET", "/paid", sig)
		assert.Equal(t, 200, retry.Code, "the connection error reached the gate: the claim was released")
		assert.Equal(t, 402, Do(h, "GET", "/paid", sig).Code)
		assert.Equal(t, int32(2), p.hits.Load())
	})

	t.Run("a writer without Flush or Unwrap in front does not break delivery", func(t *testing.T) {
		var p probe
		_, h := setup(t, p.ok)
		sig := pay(t, h)
		recorder := httptest.NewRecorder()
		ServeThrough(h, PlainWriter{recorder}, "GET", "/paid", sig)
		assert.Equal(t, 200, recorder.Code)
		assert.JSONEq(t, `{"data":42}`, recorder.Body.String())
		assert.Equal(t, 402, Do(h, "GET", "/paid", sig).Code, "the delivery was marked: the replay is the final 402")
		unpaid := httptest.NewRecorder()
		ServeThrough(h, PlainWriter{unpaid}, "GET", "/paid", "")
		assert.Equal(t, 402, unpaid.Code, "the offer is served too")
	})

	t.Run("Close waits for a paid request in flight", func(t *testing.T) {
		f := StartFacilitator(t)
		g := NewGate(t, f)
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		h := build(t, g.Middleware(), func(c Ctx) {
			once.Do(func() { close(entered) })
			<-release
			c.Respond(200, "ok")
		})
		sig := SignatureFor(t, h, PreparedTx)
		done := make(chan int, 1)
		go func() { done <- Do(h, "GET", "/paid", sig).Code }()
		<-entered
		closed := make(chan error, 1)
		go func() { closed <- g.Close(context.Background()) }()
		select {
		case <-closed: //nolint:errcheck // the channel carries the error; here it must not be ready
			t.Fatal("Close returned while a paid request was running")
		case <-time.After(100 * time.Millisecond):
		}
		close(release)
		assert.Equal(t, 200, <-done)
		select {
		case err := <-closed:
			assert.NoError(t, err)
		case <-time.After(2 * time.Second):
			t.Fatal("Close did not return")
		}
	})
}
