package canton

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	testPayTo       = "merchant::1220bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testFeePayer    = "ftp-x402-infrastructure::1220da867ceb28e1531ebfbea9ca9b25f75be06755999f48f0ec4b96f4174e3cd133"
	testSyncID      = "global-domain::1220b1431ef217342db44d516bb9befde802be7d8899637d290895fa58880f19accc"
	testTokenAdmin  = "cbtc-network::12205af3b949a04776fc48cdcc05a060f6bda2e470632935f375d1049a8546a3b262"
	testPayer       = "agent::1220cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	testUpdateID    = "1220aabbccdd0011"
	testPreparedTx  = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	testNetworkName = "canton:mainnet"
)

func TestMain(m *testing.M) {
	settleBackoff = []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond}
	retryAfterCap = 60 * time.Millisecond
	supportedRetryPause = 5 * time.Millisecond
	os.Exit(m.Run())
}

// mockFacilitator is an httptest facilitator that answers like settle.ts in
// spec mode. Each endpoint is scripted per test; calls are counted and the
// settle bodies are kept so a test can check the retries send identical bytes.
type mockFacilitator struct {
	URL string

	verifyCalls    atomic.Int32
	settleCalls    atomic.Int32
	supportedCalls atomic.Int32

	mu           sync.Mutex
	settleBodies [][]byte
	settleTimes  []time.Time
	order        []string

	supported func(n int32, w http.ResponseWriter)
	verify    func(n int32, w http.ResponseWriter)
	settle    func(n int32, w http.ResponseWriter)
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func startMock(t *testing.T) *mockFacilitator {
	t.Helper()
	f := &mockFacilitator{
		supported: func(_ int32, w http.ResponseWriter) { reply(w, 200, supportedBody(nil)) },
		verify:    func(_ int32, w http.ResponseWriter) { verifyOK(w) },
		settle:    func(_ int32, w http.ResponseWriter) { settleOK(w) },
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /supported", func(w http.ResponseWriter, _ *http.Request) {
		f.supported(f.supportedCalls.Add(1), w)
	})
	mux.HandleFunc("POST /verify", func(w http.ResponseWriter, _ *http.Request) {
		f.record("verify")
		f.verify(f.verifyCalls.Add(1), w)
	})
	mux.HandleFunc("POST /settle", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.settleBodies = append(f.settleBodies, body)
		f.settleTimes = append(f.settleTimes, time.Now())
		f.mu.Unlock()
		f.record("settle")
		f.settle(f.settleCalls.Add(1), w)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	f.URL = server.URL
	return f
}

func (f *mockFacilitator) record(event string) {
	f.mu.Lock()
	f.order = append(f.order, event)
	f.mu.Unlock()
}

func (f *mockFacilitator) events() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

func supportedBody(extraInstruments []string) map[string]any {
	extra := map[string]any{"feePayer": testFeePayer, "synchronizerId": testSyncID}
	if extraInstruments != nil {
		var list []any
		for _, admin := range extraInstruments {
			list = append(list, map[string]any{"admin": admin, "instrumentId": map[string]any{"admin": admin, "id": "CBTC"}, "symbol": "CBTC"})
		}
		extra["instruments"] = list
	}
	return map[string]any{
		"kinds":      []any{map[string]any{"x402Version": 2, "scheme": "exact", "network": testNetworkName, "extra": extra}},
		"extensions": []string{},
		"signers":    map[string]any{"canton:*": []string{testFeePayer}},
	}
}

func verifyOK(w http.ResponseWriter) {
	reply(w, 200, map[string]any{"isValid": true, "payer": testPayer})
}

func verifyInvalid(w http.ResponseWriter, reason string) {
	reply(w, 200, map[string]any{"isValid": false, "invalidReason": reason, "payer": "forged::1220dd"})
}

func settleOK(w http.ResponseWriter) {
	reply(w, 200, map[string]any{"success": true, "transaction": testUpdateID, "network": testNetworkName, "payer": testPayer})
}

func settleAlreadySettled(w http.ResponseWriter, updateID string) {
	reply(w, 200, map[string]any{"success": false, "errorReason": errReasonAlreadySettled,
		"transaction": updateID, "network": testNetworkName, "payer": testPayer})
}

func settlePending(w http.ResponseWriter, retryAfter string) {
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	reply(w, 503, map[string]any{"success": false, "errorReason": "settlement_pending",
		"transaction": testPreparedTx, "network": testNetworkName, "payer": testPayer})
}

func settleRefused(w http.ResponseWriter, reason string) {
	reply(w, 200, map[string]any{"success": false, "errorReason": reason, "transaction": "", "network": testNetworkName})
}

// dropConnection closes the TCP connection without an answer: a transport error.
func dropConnection(w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		panic("no hijacker")
	}
	conn, _, err := hijacker.Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

func testRoutes() map[string]Route {
	return map[string]Route{
		"GET /paid": {PayTo: testPayTo, Price: CC("0.01"), Memo: "order-1", Description: "paid data", MimeType: "application/json"},
	}
}

// newGate builds the middleware against the mock and wraps handler.
func newGate(t *testing.T, f *mockFacilitator, handler http.HandlerFunc, mutate ...func(*Config)) http.Handler {
	t.Helper()
	cfg := Config{
		FacilitatorURL: f.URL,
		Routes:         testRoutes(),
		SupportedWait:  2 * time.Second,
		Logger:         slog.New(slog.DiscardHandler),
	}
	for _, change := range mutate {
		change(&cfg)
	}
	mw, err := NewMiddleware(context.Background(), cfg)
	require.NoError(t, err)
	return mw(handler)
}

func okHandler(hits *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":42}`))
	}
}

func do(h http.Handler, method, target, signature string) *httptest.ResponseRecorder {
	return doCtx(context.Background(), h, method, target, signature)
}

func doCtx(ctx context.Context, h http.Handler, method, target, signature string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(ctx, method, target, nil)
	r.Header.Set("Accept", "application/json")
	if signature != "" {
		r.Header.Set(headerPaymentSig, signature)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decodeHeader(t *testing.T, value string, into any) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(value)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, into))
}

// signatureFor asks the gate for its 402 and builds a payment for the given
// preparedTxHash from it, the way a client does.
func signatureFor(t *testing.T, h http.Handler, preparedTxHash string) string {
	t.Helper()
	w := do(h, "GET", "/paid", "")
	require.Equal(t, 402, w.Code, w.Body.String())
	var required map[string]any
	decodeHeader(t, w.Header().Get(headerPaymentRequired), &required)
	accepts := required["accepts"].([]any)
	envelope, err := json.Marshal(map[string]any{
		"x402Version": 2, "resource": required["resource"], "accepted": accepts[0],
		"payload": map[string]any{
			"assetTransferMethod": "transfer-factory", "preparedTransaction": "H4sIAAAA",
			"preparedTxHash": preparedTxHash, "signature": "c2lnbmF0dXJl",
		},
	})
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(envelope)
}

func paymentResponse(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	decodeHeader(t, w.Header().Get(headerPaymentResponse), &out)
	return out
}

func errorMessage(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	return body["error"]
}

func logsContain(buf *syncBuffer, parts ...string) bool {
	text := buf.String()
	for _, part := range parts {
		if !strings.Contains(text, part) {
			return false
		}
	}
	return true
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func capturingLogger() (*slog.Logger, *syncBuffer) {
	sink := &syncBuffer{}
	return slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})), sink
}

// signatureForRaw builds a header with a given preparedTxHash and no accepted
// block: enough for the hash parser, not for the SDK.
func signatureForRaw(preparedTxHash string) string {
	return signatureForPayload(map[string]any{"preparedTxHash": preparedTxHash})
}

// signatureForPayload wraps a payload map in a readable x402 v2 header.
func signatureForPayload(payload map[string]any) string {
	accepted := map[string]any{"scheme": "exact", "network": testNetworkName, "asset": "CC", "amount": "1", "payTo": testPayTo,
		"maxTimeoutSeconds": 60, "extra": map[string]any{}}
	envelope, _ := json.Marshal(map[string]any{"x402Version": 2, "accepted": accepted, "payload": payload})
	return base64.StdEncoding.EncodeToString(envelope)
}

func nopLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// rebuildSignature builds a payment for the hash from the 402 the gate answered.
func rebuildSignature(unpaid *httptest.ResponseRecorder, preparedTxHash string) string {
	var required map[string]any
	raw, _ := base64.StdEncoding.DecodeString(unpaid.Header().Get(headerPaymentRequired))
	_ = json.Unmarshal(raw, &required)
	envelope, _ := json.Marshal(map[string]any{
		"x402Version": 2, "resource": required["resource"], "accepted": required["accepts"].([]any)[0],
		"payload": map[string]any{"assetTransferMethod": "transfer-factory", "preparedTransaction": "H4sIAAAA",
			"preparedTxHash": preparedTxHash, "signature": "c2lnbmF0dXJl"},
	})
	return base64.StdEncoding.EncodeToString(envelope)
}
