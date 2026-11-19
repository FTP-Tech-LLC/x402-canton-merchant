package canton

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The lifetime a charged request gets when its decoded path, past "?", cleans to
// another key's path.
func TestZ2BudgetOfAChargedSpelling(t *testing.T) {
	f := startMock(t)
	cfg := Config{FacilitatorURL: f.URL, SupportedWait: 2 * time.Second, Logger: slog.New(slog.DiscardHandler)}
	z2Routes(&cfg)
	g, err := buildGate(context.Background(), cfg)
	require.NoError(t, err)
	for target, wantLifetime := range map[string]time.Duration{
		"/paid":                         10 * time.Minute,
		"/paid%3F/../free2":             5 * time.Second,
		"/paid%3F%252F..%252Ffree2":     5 * time.Second,
		"/paid%3F%25252F..%25252Ffree2": 5 * time.Second,
	} {
		r := httptest.NewRequest("GET", target, nil)
		require.True(t, g.httpServer.RequiresPayment(g.requestContext(r)), target)
		require.Equal(t, wantLifetime, g.routes.executeBefore(r), "the lifetime of the key the spelling is charged as: "+target)
		require.GreaterOrEqual(t, g.budgetFor(r), g.routes.executeBefore(r), "the budget covers the lifetime: "+target)
	}
}
