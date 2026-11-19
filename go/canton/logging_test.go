package canton

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Rule 16: slog only; the payment header and the signature are never logged.

func TestNothingSecretIsEverLogged(t *testing.T) {
	scenarios := map[string]func(f *mockFacilitator, c *Config){
		"success": func(*mockFacilitator, *Config) {},
		"unknown outcome": func(f *mockFacilitator, _ *Config) {
			f.settle = func(_ int32, w http.ResponseWriter) { w.WriteHeader(502) }
		},
		"verify outage": func(f *mockFacilitator, _ *Config) {
			f.verify = func(_ int32, w http.ResponseWriter) { w.WriteHeader(502) }
		},
		"store failure": func(_ *mockFacilitator, c *Config) {
			store := newFaultyStore(NewMemoryStore())
			store.breakOp("claim", true)
			c.Store = store
		},
		"redirect": func(*mockFacilitator, *Config) {},
	}
	for name, setup := range scenarios {
		t.Run(name, func(t *testing.T) {
			f := startMock(t)
			logger, logs := capturingLogger()
			handler := okHandler(new(atomic.Int32))
			if name == "redirect" {
				handler = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(302) }
			}
			h := newGate(t, f, handler, func(c *Config) { c.Logger = logger; setup(f, c) })
			sig := signatureFor(t, h, testPreparedTx)
			do(h, "GET", "/paid", sig)

			output := logs.String()
			require.NotEmpty(t, output, "the scenario logs something")
			assert.NotContains(t, output, sig, "the whole header")
			assert.NotContains(t, output, "c2lnbmF0dXJl", "the signature value")
			assert.NotContains(t, output, "H4sIAAAA", "the prepared transaction")
			assert.NotContains(t, output, headerPaymentSig)
		})
	}
}
