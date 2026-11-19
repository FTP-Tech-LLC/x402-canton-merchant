package canton

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cantonmech "github.com/FTP-Tech-LLC/x402-canton-merchant/go/mechanisms/canton"
)

// Rule 3: the 402 offer, built by the SDK through the mirror scheme.

func offerFrom(t *testing.T, h http.Handler, target string) (required map[string]any, accepted map[string]any) {
	t.Helper()
	w := do(h, "GET", target, "")
	require.Equal(t, 402, w.Code, w.Body.String())
	decodeHeader(t, w.Header().Get(headerPaymentRequired), &required)
	accepted = required["accepts"].([]any)[0].(map[string]any)
	return required, accepted
}

func TestOfferCarriesTheCantonTermsFromSupported(t *testing.T) {
	f := startMock(t)
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) {
		c.Routes["GET /paid"] = Route{PayTo: testPayTo, Price: CC("0.01"), ExecuteBeforeSeconds: 90, Memo: "order-1",
			Description: "paid data", MimeType: "application/json"}
	})
	required, accepted := offerFrom(t, h, "/paid")

	assert.EqualValues(t, 2, required["x402Version"])
	assert.Equal(t, "exact", accepted["scheme"])
	assert.Equal(t, "canton:mainnet", accepted["network"])
	assert.Equal(t, "100000000", accepted["amount"], "atomic integer, 10 decimals")
	assert.Equal(t, "CC", accepted["asset"])
	assert.Equal(t, testPayTo, accepted["payTo"])

	extra := accepted["extra"].(map[string]any)
	assert.Equal(t, "transfer-factory", extra["assetTransferMethod"])
	assert.Equal(t, testFeePayer, extra["feePayer"], "feePayer comes from /supported")
	assert.Equal(t, testSyncID, extra["synchronizerId"], "synchronizerId comes from /supported")
	assert.Equal(t, map[string]any{"admin": DSOMainnetParty, "id": "Amulet"}, extra["instrumentId"])
	assert.EqualValues(t, 90, extra["executeBeforeSeconds"])
	assert.Equal(t, "order-1", extra["memo"])
	assert.NotContains(t, extra, "paymentFlow", "authorization flow only: upfront is never offered")
	assert.Equal(t, int32(0), f.verifyCalls.Load())
	assert.Equal(t, int32(0), f.settleCalls.Load())

	resource := required["resource"].(map[string]any)
	assert.Equal(t, "paid data", resource["description"])
	assert.Equal(t, "application/json", resource["mimeType"])
}

func TestOfferDefaultsExecuteBeforeAndOmitsMemo(t *testing.T) {
	f := startMock(t)
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) {
		c.Routes["GET /paid"] = Route{PayTo: testPayTo, Price: CC("1")}
	})
	_, accepted := offerFrom(t, h, "/paid")
	extra := accepted["extra"].(map[string]any)
	assert.EqualValues(t, 120, extra["executeBeforeSeconds"])
	assert.NotContains(t, extra, "memo")
	assert.Equal(t, "10000000000", accepted["amount"])
}

func TestOfferForATokenUsesTheStructuredAsset(t *testing.T) {
	f := startMock(t)
	f.supported = func(_ int32, w http.ResponseWriter) { reply(w, 200, supportedBody([]string{testTokenAdmin})) }
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) {
		c.Routes["GET /paid"] = Route{PayTo: testPayTo, Price: Token(testTokenAdmin, "CBTC", "0.5")}
	})
	_, accepted := offerFrom(t, h, "/paid")
	assert.Equal(t, testTokenAdmin+"::CBTC", accepted["asset"])
	assert.Equal(t, "5000000000", accepted["amount"])
	assert.Equal(t, map[string]any{"admin": testTokenAdmin, "id": "CBTC"}, accepted["extra"].(map[string]any)["instrumentId"])
}

func TestOfferPricesEachRouteSeparately(t *testing.T) {
	f := startMock(t)
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) {
		c.Routes["GET /v1"] = Route{PayTo: testPayTo, Price: CC("0.01")}
		c.Routes["GET /v1/premium"] = Route{PayTo: testPayTo, Price: CC("0.09")}
	})
	for i := 0; i < 20; i++ { // the SDK matches routes in map order: literal keys cannot overlap
		_, cheap := offerFrom(t, h, "/v1")
		_, premium := offerFrom(t, h, "/v1/premium")
		require.Equal(t, "100000000", cheap["amount"])
		require.Equal(t, "900000000", premium["amount"])
	}
}

// TestOfferLive checks the same offer against the production facilitator. It
// only reads GET /supported and skips when the network is not reachable.
func TestOfferLive(t *testing.T) {
	if os.Getenv("CANTON_MERCHANT_OFFLINE") != "" {
		t.Skip("CANTON_MERCHANT_OFFLINE is set")
	}
	probe, err := http.NewRequestWithContext(context.Background(), http.MethodGet, DefaultFacilitatorURL+"/supported", nil)
	require.NoError(t, err)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(probe)
	if err != nil {
		t.Skipf("facilitator not reachable: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("facilitator /supported answered %d", resp.StatusCode)
	}

	mw, err := NewMiddleware(context.Background(), Config{Routes: testRoutes(), SupportedWait: 20 * time.Second})
	require.NoError(t, err, "the live /supported must still carry an exact canton:mainnet kind with feePayer and synchronizerId")
	w := httptest.NewRecorder()
	mw(http.NotFoundHandler()).ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), "GET", "/paid", nil))
	require.Equal(t, 402, w.Code)
	var required map[string]any
	decodeHeader(t, w.Header().Get(headerPaymentRequired), &required)
	accepted := required["accepts"].([]any)[0].(map[string]any)
	extra := accepted["extra"].(map[string]any)
	assert.Equal(t, cantonmech.CantonMainnetCAIP2, accepted["network"])
	assert.Equal(t, "100000000", accepted["amount"])
	assert.Equal(t, "transfer-factory", extra["assetTransferMethod"])
	assert.NotEmpty(t, extra["feePayer"])
	assert.NotEmpty(t, extra["synchronizerId"])
	assert.Equal(t, map[string]any{"admin": DSOMainnetParty, "id": "Amulet"}, extra["instrumentId"])
}
