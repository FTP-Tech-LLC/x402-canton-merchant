package canton

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cantonmech "github.com/FTP-Tech-LLC/x402-canton-merchant/go/mechanisms/canton"
)

// Rule 2: route keys are literal; fields are validated; unknown token admins are refused.

func validRoute() Route { return Route{PayTo: testPayTo, Price: CC("0.01")} }

func TestRouteKeysMustBeMethodAndLiteralPath(t *testing.T) {
	invalidKeys := []string{
		"", "/paid", "GET", "GET  /paid", " GET /paid", "GET /paid ", "get /paid", "HEAD /paid", "OPTIONS /paid", "*  /paid",
		"GET paid", "GET /paid/*", "GET /paid/:id", "GET /paid/[id]", "GET /paid?x=1", "GET /paid#frag", "GET /a b",
		"GET /paid/", "GET //paid", "GET /", "GET /items/{id}", "GET /files/{path...}", "GET /{$}", "GET /a/{$}", "GET /a$", "GET /a/./b", "GET /a/../b", "GET /%70aid", "GET /pa\\id", "GET /päid", "GET\t/paid",
	}
	for _, key := range invalidKeys {
		t.Run(key, func(t *testing.T) {
			_, problems := checkRoutes(map[string]Route{key: validRoute()})
			assert.NotEmpty(t, problems, "key %q must be refused", key)
		})
	}
	for _, key := range []string{"GET /paid", "POST /v1/data", "PUT /a", "PATCH /a-b_c.d", "DELETE /x/y/z"} {
		_, problems := checkRoutes(map[string]Route{key: validRoute()})
		assert.Empty(t, problems, "key %q must be accepted", key)
	}
}

func TestRoutesRefuseDuplicatePathsIgnoringCase(t *testing.T) {
	_, problems := checkRoutes(map[string]Route{"GET /Paid": validRoute(), "GET /paid": validRoute()})
	require.Len(t, problems, 1)
	assert.Contains(t, problems[0].Error(), "duplicates")
	_, problems = checkRoutes(map[string]Route{"GET /paid": validRoute(), "POST /paid": validRoute()})
	assert.Empty(t, problems, "same path with another method is a different route")
}

func TestRoutesRefuseEmptyRoutes(t *testing.T) {
	_, problems := checkRoutes(nil)
	assert.NotEmpty(t, problems)
}

func TestRouteFieldValidation(t *testing.T) {
	long := strings.Repeat("a", 257)
	cases := map[string]Route{
		"payTo without 1220":     {PayTo: "merchant::abcd", Price: CC("1")},
		"payTo short hex":        {PayTo: "merchant::1220bb", Price: CC("1")},
		"payTo upper hex":        {PayTo: "merchant::1220" + strings.Repeat("B", 64), Price: CC("1")},
		"payTo empty":            {Price: CC("1")},
		"payTo no hint":          {PayTo: "::1220" + strings.Repeat("b", 64), Price: CC("1")},
		"price unset":            {PayTo: testPayTo},
		"price zero":             {PayTo: testPayTo, Price: CC("0")},
		"price negative":         {PayTo: testPayTo, Price: CC("-1")},
		"price too precise":      {PayTo: testPayTo, Price: CC("0.00000000001")},
		"price money string":     {PayTo: testPayTo, Price: CC("$0.01")},
		"atomic leading zero":    {PayTo: testPayTo, Price: Atomic("0100", cantonmech.InstrumentID{Admin: "a", ID: "b"})},
		"atomic decimal":         {PayTo: testPayTo, Price: Atomic("1.5", cantonmech.InstrumentID{Admin: "a", ID: "b"})},
		"atomic zero":            {PayTo: testPayTo, Price: Atomic("0", cantonmech.InstrumentID{Admin: "a", ID: "b"})},
		"atomic too long":        {PayTo: testPayTo, Price: Atomic(strings.Repeat("9", 39), cantonmech.InstrumentID{Admin: "a", ID: "b"})},
		"atomic no instrument":   {PayTo: testPayTo, Price: Atomic("5", cantonmech.InstrumentID{})},
		"executeBefore too big":  {PayTo: testPayTo, Price: CC("1"), ExecuteBeforeSeconds: 601},
		"executeBefore negative": {PayTo: testPayTo, Price: CC("1"), ExecuteBeforeSeconds: -1},
		"memo too long":          {PayTo: testPayTo, Price: CC("1"), Memo: long},
		"memo leading space":     {PayTo: testPayTo, Price: CC("1"), Memo: " x"},
		"memo trailing newline":  {PayTo: testPayTo, Price: CC("1"), Memo: "x\n"},
		"memo bad utf8":          {PayTo: testPayTo, Price: CC("1"), Memo: "\xff"},
	}
	for name, route := range cases {
		t.Run(name, func(t *testing.T) {
			_, problems := checkRoutes(map[string]Route{"GET /paid": route})
			assert.NotEmpty(t, problems)
		})
	}
}

func TestRouteFieldBoundariesAreAccepted(t *testing.T) {
	good := []Route{
		{PayTo: testPayTo, Price: CC("0.0000000001")},
		{PayTo: testPayTo, Price: CC("12345.6789")},
		{PayTo: testPayTo, Price: CC("1"), ExecuteBeforeSeconds: 1},
		{PayTo: testPayTo, Price: CC("1"), ExecuteBeforeSeconds: 600},
		{PayTo: testPayTo, Price: CC("1"), Memo: strings.Repeat("a", 256)},
		{PayTo: testPayTo, Price: CC("1"), Memo: strings.Repeat("é", 128)},
		{PayTo: testPayTo, Price: Atomic(strings.Repeat("9", 38), cantonmech.InstrumentID{Admin: "a", ID: "b"})},
	}
	for index, route := range good {
		_, problems := checkRoutes(map[string]Route{"GET /paid": route})
		assert.Empty(t, problems, "case %d", index)
	}
}

func TestRouteProblemsAreReportedTogether(t *testing.T) {
	_, err := NewMiddleware(context.Background(), Config{Routes: map[string]Route{
		"GET /a/*": validRoute(), "GET /b": {PayTo: "bad", Price: CC("1")},
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/a/*")
	assert.Contains(t, err.Error(), "PayTo")
}

func TestPriceConversions(t *testing.T) {
	cases := map[string]string{"0.01": "100000000", "1": "10000000000", "1.5": "15000000000", "0.0000000001": "1", "007.10": "71000000000"}
	for decimal, want := range cases {
		price := CC(decimal)
		require.NoError(t, price.validate(), decimal)
		assert.Equal(t, want, price.atomic, decimal)
	}
	assert.Equal(t, "CC", CC("1").asset())
	token := Token(testTokenAdmin, "CBTC", "2")
	assert.Equal(t, testTokenAdmin+"::CBTC", token.asset())
	assert.False(t, token.isCantonCoin())
	assert.True(t, Atomic("5", cantonmech.InstrumentID{Admin: DSOMainnetParty, ID: "Amulet"}).isCantonCoin())
}

func TestTokenAdminMustBeListedBySupported(t *testing.T) {
	routes := map[string]Route{"GET /paid": {PayTo: testPayTo, Price: Token("unknown::1220"+strings.Repeat("a", 64), "CBTC", "1")}}
	f := startMock(t)
	f.supported = func(_ int32, w http.ResponseWriter) { reply(w, 200, supportedBody([]string{testTokenAdmin})) }
	_, err := NewMiddleware(context.Background(), Config{FacilitatorURL: f.URL, Routes: routes, SupportedWait: time.Second})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in the facilitator")

	routes["GET /paid"] = Route{PayTo: testPayTo, Price: Token(testTokenAdmin, "CBTC", "1")}
	_, err = NewMiddleware(context.Background(), Config{FacilitatorURL: f.URL, Routes: routes, SupportedWait: time.Second})
	require.NoError(t, err)
}

func TestTokenAdminCheckSkippedWhenNoInstrumentsListed(t *testing.T) {
	f := startMock(t) // default /supported lists no instruments
	routes := map[string]Route{"GET /paid": {PayTo: testPayTo, Price: Token("any::1220"+strings.Repeat("a", 64), "X", "1")}}
	_, err := NewMiddleware(context.Background(), Config{FacilitatorURL: f.URL, Routes: routes, SupportedWait: time.Second})
	require.NoError(t, err)
}

func TestCantonCoinNeedsNoInstrumentListing(t *testing.T) {
	f := startMock(t)
	f.supported = func(_ int32, w http.ResponseWriter) { reply(w, 200, supportedBody([]string{testTokenAdmin})) }
	newGate(t, f, okHandler(new(atomic.Int32)))
}

func TestConfigDefaults(t *testing.T) {
	conf, err := Config{Routes: map[string]Route{
		"GET /a": validRoute(),
		"GET /b": {PayTo: testPayTo, Price: CC("1"), ExecuteBeforeSeconds: 300},
	}}.resolve()
	require.NoError(t, err)
	assert.Equal(t, DefaultFacilitatorURL, conf.FacilitatorURL)
	assert.Equal(t, 300*time.Second, conf.Timeout, "default Timeout is the largest route ExecuteBeforeSeconds")
	assert.Equal(t, 65*time.Second, conf.SettleTimeout)
	assert.Equal(t, 4, conf.SettleCalls)
	assert.Equal(t, 60*time.Second, conf.SupportedWait)
	assert.False(t, conf.SettleOnRedirect)
	assert.IsType(t, &memoryStore{}, conf.Store)
	assert.NotNil(t, conf.Logger)
	assert.NotNil(t, conf.HTTPClient)

	conf, err = Config{Routes: map[string]Route{"GET /a": validRoute()}}.resolve()
	require.NoError(t, err)
	assert.Equal(t, 120*time.Second, conf.Timeout)
	assert.Equal(t, 120, conf.routes[0].executeBeforeSeconds)
}

func TestConfigRefusesNegativeValues(t *testing.T) {
	for name, change := range map[string]func(*Config){
		"timeout":       func(c *Config) { c.Timeout = -1 },
		"settleTimeout": func(c *Config) { c.SettleTimeout = -1 },
		"settleCalls":   func(c *Config) { c.SettleCalls = -1 },
		"supportedWait": func(c *Config) { c.SupportedWait = -1 },
	} {
		cfg := Config{Routes: map[string]Route{"GET /a": validRoute()}}
		change(&cfg)
		_, err := cfg.resolve()
		assert.Error(t, err, name)
	}
}
