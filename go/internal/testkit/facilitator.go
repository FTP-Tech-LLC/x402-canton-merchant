// Package testkit is the test harness shared by the framework adapters: a mock
// facilitator that answers like the real one in spec mode, payment builders, a
// fake Redis, and the money matrix every adapter must pass. It is internal and
// only imported by tests.
package testkit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
)

const (
	PayTo      = "merchant::1220bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	FeePayer   = "ftp-x402-infrastructure::1220da867ceb28e1531ebfbea9ca9b25f75be06755999f48f0ec4b96f4174e3cd133"
	SyncID     = "global-domain::1220b1431ef217342db44d516bb9befde802be7d8899637d290895fa58880f19accc"
	Payer      = "agent::1220cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	UpdateID   = "1220aabbccdd0011"
	PreparedTx = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	Network    = "canton:mainnet"
)

// Facilitator is a scriptable httptest facilitator.
type Facilitator struct {
	URL string

	VerifyCalls atomic.Int32
	SettleCalls atomic.Int32

	mu     sync.Mutex
	verify func(n int32, w http.ResponseWriter)
	settle func(n int32, w http.ResponseWriter)
}

// Reply writes a JSON answer.
func Reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// StartFacilitator starts a facilitator whose /verify is valid and /settle succeeds.
func StartFacilitator(t *testing.T) *Facilitator {
	t.Helper()
	f := &Facilitator{
		verify: func(_ int32, w http.ResponseWriter) { VerifyOK(w) },
		settle: func(_ int32, w http.ResponseWriter) { SettleOK(w) },
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /supported", func(w http.ResponseWriter, _ *http.Request) {
		Reply(w, 200, map[string]any{
			"kinds": []any{map[string]any{"x402Version": 2, "scheme": "exact", "network": Network,
				"extra": map[string]any{"feePayer": FeePayer, "synchronizerId": SyncID}}},
			"extensions": []string{}, "signers": map[string]any{"canton:*": []string{FeePayer}},
		})
	})
	mux.HandleFunc("POST /verify", func(w http.ResponseWriter, _ *http.Request) {
		n := f.VerifyCalls.Add(1)
		f.mu.Lock()
		fn := f.verify
		f.mu.Unlock()
		fn(n, w)
	})
	mux.HandleFunc("POST /settle", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		n := f.SettleCalls.Add(1)
		f.mu.Lock()
		fn := f.settle
		f.mu.Unlock()
		fn(n, w)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	f.URL = server.URL
	return f
}

// OnVerify scripts /verify.
func (f *Facilitator) OnVerify(fn func(n int32, w http.ResponseWriter)) {
	f.mu.Lock()
	f.verify = fn
	f.mu.Unlock()
}

// OnSettle scripts /settle.
func (f *Facilitator) OnSettle(fn func(n int32, w http.ResponseWriter)) {
	f.mu.Lock()
	f.settle = fn
	f.mu.Unlock()
}

func VerifyOK(w http.ResponseWriter) { Reply(w, 200, map[string]any{"isValid": true, "payer": Payer}) }

func VerifyInvalid(w http.ResponseWriter, reason string) {
	Reply(w, 200, map[string]any{"isValid": false, "invalidReason": reason, "payer": "forged::1220dd"})
}

func SettleOK(w http.ResponseWriter) {
	Reply(w, 200, map[string]any{"success": true, "transaction": UpdateID, "network": Network, "payer": Payer})
}

// SettleOKWith is SettleOK with a chosen updateId, for tests that settle more
// than one payment on one gate.
func SettleOKWith(w http.ResponseWriter, updateID string) {
	Reply(w, 200, map[string]any{"success": true, "transaction": updateID, "network": Network, "payer": Payer})
}

func SettleAlreadySettled(w http.ResponseWriter) {
	Reply(w, 200, map[string]any{"success": false, "errorReason": "invalid_exact_canton_payment_already_settled",
		"transaction": UpdateID, "network": Network, "payer": Payer})
}

func SettlePending(w http.ResponseWriter) {
	Reply(w, 503, map[string]any{"success": false, "errorReason": "settlement_pending", "transaction": PreparedTx, "network": Network})
}

func SettleRefused(w http.ResponseWriter, reason string) {
	Reply(w, 200, map[string]any{"success": false, "errorReason": reason, "transaction": "", "network": Network})
}

// Routes is the paid route every matrix test uses: GET /paid for 0.01 CC.
func Routes() map[string]canton.Route {
	return map[string]canton.Route{"GET /paid": {PayTo: PayTo, Price: canton.CC("0.01")}}
}

// NewGate builds a canton gate against the facilitator and closes it with the test.
func NewGate(t *testing.T, f *Facilitator, mutate ...func(*canton.Config)) *canton.Gate {
	t.Helper()
	cfg := canton.Config{FacilitatorURL: f.URL, Routes: Routes(), SupportedWait: 2 * time.Second,
		Logger: slog.New(slog.DiscardHandler)}
	for _, change := range mutate {
		change(&cfg)
	}
	g, err := canton.NewGate(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close(context.Background()) })
	return g
}

// Do sends a request through a handler.
func Do(h http.Handler, method, target, signature string) *httptest.ResponseRecorder {
	return DoCtx(context.Background(), h, method, target, signature)
}

// DoCtx is Do with a request context (cancel it to simulate a payer who left).
func DoCtx(ctx context.Context, h http.Handler, method, target, signature string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(ctx, method, target, nil)
	r.Header.Set("Accept", "application/json")
	if signature != "" {
		r.Header.Set("PAYMENT-SIGNATURE", signature)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// DecodeHeader decodes a base64 JSON header value.
func DecodeHeader(t *testing.T, value string, into any) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(value)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, into))
}

// SignatureFor asks a handler for its 402 on GET /paid and builds a payment for
// the given preparedTxHash from it, as a client does.
func SignatureFor(t *testing.T, h http.Handler, preparedTxHash string) string {
	t.Helper()
	return SignatureForPath(t, h, "/paid", preparedTxHash)
}

// SignatureForPath is SignatureFor for another GET path.
func SignatureForPath(t *testing.T, h http.Handler, path, preparedTxHash string) string {
	t.Helper()
	w := Do(h, "GET", path, "")
	require.Equal(t, 402, w.Code, w.Body.String())
	var required map[string]any
	DecodeHeader(t, w.Header().Get("PAYMENT-REQUIRED"), &required)
	envelope, err := json.Marshal(map[string]any{
		"x402Version": 2, "resource": required["resource"], "accepted": required["accepts"].([]any)[0],
		"payload": map[string]any{"assetTransferMethod": "transfer-factory", "preparedTransaction": "H4sIAAAA",
			"preparedTxHash": preparedTxHash, "signature": "c2lnbmF0dXJl"},
	})
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(envelope)
}

// PaymentResponse decodes the PAYMENT-RESPONSE header.
func PaymentResponse(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	DecodeHeader(t, w.Header().Get("PAYMENT-RESPONSE"), &out)
	return out
}

// FakeRedis is an in-memory canton.RedisClient with real SET NX and expiry.
type FakeRedis struct {
	mu     sync.Mutex
	values map[string]fakeValue
}

type fakeValue struct {
	value   string
	expires time.Time
}

func NewFakeRedis() *FakeRedis { return &FakeRedis{values: map[string]fakeValue{}} }

func (r *FakeRedis) live(key string) (fakeValue, bool) {
	entry, found := r.values[key]
	if !found || !time.Now().Before(entry.expires) {
		delete(r.values, key)
		return fakeValue{}, false
	}
	return entry, true
}

func (r *FakeRedis) SetNX(_ context.Context, key, value string, ttl time.Duration) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, found := r.live(key); found {
		return false, nil
	}
	r.values[key] = fakeValue{value, time.Now().Add(ttl)}
	return true, nil
}

func (r *FakeRedis) Set(_ context.Context, key, value string, ttl time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[key] = fakeValue{value, time.Now().Add(ttl)}
	return nil
}

func (r *FakeRedis) Get(_ context.Context, key string) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, found := r.live(key)
	return entry.value, found, nil
}

func (r *FakeRedis) Del(_ context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.values, key)
	return nil
}

func (r *FakeRedis) DelIfValue(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, found := r.live(key); found && entry.value == value {
		delete(r.values, key)
	}
	return nil
}

// ErrBroken is the error of a connection that broke while the answer was flushed.
var ErrBroken = errors.New("write: broken pipe")

// BrokenFlushWriter is a ResponseWriter whose flush fails like a dropped
// connection. Its Flush drops the error, as net/http's response does: only
// FlushError (what http.NewResponseController uses first) reports it.
type BrokenFlushWriter struct{ *httptest.ResponseRecorder }

func (w BrokenFlushWriter) FlushError() error { return ErrBroken }
func (w BrokenFlushWriter) Flush()            {}

// PlainWriter hides everything but the http.ResponseWriter methods: no Flush,
// no Unwrap (http.TimeoutHandler, a hand-written status recorder).
type PlainWriter struct{ http.ResponseWriter }

// ServeThrough serves one request through a handler with a chosen writer.
func ServeThrough(h http.Handler, w http.ResponseWriter, method, target, signature string) {
	r := httptest.NewRequestWithContext(context.Background(), method, target, nil)
	r.Header.Set("Accept", "application/json")
	if signature != "" {
		r.Header.Set("PAYMENT-SIGNATURE", signature)
	}
	h.ServeHTTP(w, r)
}
