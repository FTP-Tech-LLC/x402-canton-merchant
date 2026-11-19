package gin_test

// Round-2 audit probe (lens: gin): a real HTTP/2 connection. The payer cancels
// the stream after the claim was taken and before the answer is flushed (the
// claim is held in the Store long enough to cancel). http2responseWriter's
// FlushError reports the closed stream; the gate must give the claim back.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// slowClaimStore holds the first Claim until release is closed.
type slowClaimStore struct {
	canton.Store
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (s *slowClaimStore) Claim(ctx context.Context, updateID, token string, ttl time.Duration) (bool, error) {
	first := false
	s.once.Do(func() { first = true })
	if first {
		close(s.entered)
		<-s.release
	}
	return s.Store.Claim(ctx, updateID, token, ttl)
}

func r2H2CancelAfterClaim(t *testing.T, wrap gingonic.HandlerFunc) (retry int, body string) {
	t.Helper()
	f := testkit.StartFacilitator(t)
	f.OnSettle(func(n int32, w http.ResponseWriter) {
		if n == 1 {
			testkit.SettleOK(w)
			return
		}
		testkit.SettleAlreadySettled(w)
	})
	store := &slowClaimStore{Store: canton.NewMemoryStore(), entered: make(chan struct{}), release: make(chan struct{})}
	g := testkit.NewGate(t, f, func(c *canton.Config) { c.Store = store })
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	if wrap != nil {
		r.Use(wrap)
	}
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { c.String(200, "paid content") })

	srv := httptest.NewUnstartedServer(r)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	client := srv.Client()

	sig := testkit.SignatureFor(t, r, testkit.PreparedTx)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/paid", nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("PAYMENT-SIGNATURE", sig)
	done := make(chan error, 1)
	go func() {
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			t.Logf("first answer: proto=%s status=%d", resp.Proto, resp.StatusCode)
		}
		done <- err
	}()
	<-store.entered                    // settled, the claim is being taken
	cancel()                           // the payer cancels the stream (RST_STREAM)
	time.Sleep(200 * time.Millisecond) // the server processes the reset
	close(store.release)
	t.Logf("first request: %v", <-done)
	time.Sleep(300 * time.Millisecond) // post-response work (unclaim or delivered mark)

	again, _ := http.NewRequest("GET", srv.URL+"/paid", nil)
	again.Header.Set("Accept", "application/json")
	again.Header.Set("PAYMENT-SIGNATURE", sig)
	resp, err := client.Do(again)
	require.NoError(t, err)
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(b)
}

func TestR2H2CancelAfterClaimNoWrapperControl(t *testing.T) {
	code, body := r2H2CancelAfterClaim(t, nil)
	t.Logf("h2, gin's own writer: retry=%d %q", code, body)
	assert.Equal(t, 200, code, "the retry of an undelivered payment is served once")
}

func TestR2H2CancelAfterClaimThroughUnwrapWrapper(t *testing.T) {
	code, body := r2H2CancelAfterClaim(t, func(c *gingonic.Context) {
		c.Writer = &r2UnwrapWrapper{ResponseWriter: c.Writer}
		c.Next()
	})
	t.Logf("h2, Unwrap wrapper outside the gate: retry=%d %q", code, body)
	assert.Equal(t, 200, code, "the retry of an undelivered payment is served once")
}
