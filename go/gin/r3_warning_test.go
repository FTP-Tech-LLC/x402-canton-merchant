package gin_test

// Round-3 audit probes (lens: core flush + gin): the once-per-process warning
// "the gate cannot see the connection". The gate warns on every gated flush it
// cannot see, also an unpaid 402 offer written into a synthetic writer: Check's
// recorder at startup and the discard writer of the unpaid-clone check. That
// spends the one warning on a correct setup, and a real blind wrapper added
// later is never reported. Each probe runs in a child process, because the flag
// is a process global.

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

const r3BlindMsg = "cannot see the connection"

type r3SafeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *r3SafeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *r3SafeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// r3InChild runs the named test in a fresh process and returns its output.
func r3InChild(t *testing.T, name string) (string, bool) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "R3_CHILD=1")
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

// r3BlindBodyLogger is the common gin body logger: embeds gin.ResponseWriter,
// overrides Write, no Unwrap. The gate cannot see the connection through it.
type r3BlindBodyLogger struct {
	gingonic.ResponseWriter
}

func (w *r3BlindBodyLogger) Write(b []byte) (int, error) { return w.ResponseWriter.Write(b) }

type r3BrokenConn struct{ *httptest.ResponseRecorder }

func (w r3BrokenConn) FlushError() error { return io.ErrClosedPipe }
func (w r3BrokenConn) Flush()            {}

// The README setup with no wrapper: Check at startup must not log the
// blind-flush warning, and a blind wrapper served later must still be reported.
func TestR3CheckDoesNotSpendTheBlindFlushWarning(t *testing.T) {
	if os.Getenv("R3_CHILD") == "" {
		out, ok := r3InChild(t, "TestR3CheckDoesNotSpendTheBlindFlushWarning")
		t.Log(out)
		assert.True(t, ok, "child failed")
		return
	}
	logs := &r3SafeBuffer{}
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f, func(c *canton.Config) { c.Logger = slog.New(slog.NewTextHandler(logs, nil)) })

	correct := gingonic.New()
	correct.Use(gingonic.Recovery())
	correct.Use(cantongin.Middleware(g.Middleware()))
	correct.GET("/paid", func(c *gingonic.Context) { c.String(200, "paid content") })
	require.NoError(t, cantongin.Check(correct, testkit.Routes()))
	t.Logf("after Check on a correct setup: warnings=%d logs=%q", strings.Count(logs.String(), r3BlindMsg), logs.String())
	assert.NotContains(t, logs.String(), r3BlindMsg, "a correct setup is warned about at startup")

	// Later the merchant adds a blind body logger in front of the gate.
	blind := gingonic.New()
	blind.Use(gingonic.Recovery())
	blind.Use(func(c *gingonic.Context) { c.Writer = &r3BlindBodyLogger{c.Writer}; c.Next() })
	blind.Use(cantongin.Middleware(g.Middleware()))
	blind.GET("/paid", func(c *gingonic.Context) { c.String(200, "paid content") })
	sig := testkit.SignatureFor(t, blind, testkit.PreparedTx)
	before := strings.Count(logs.String(), r3BlindMsg)
	req := httptest.NewRequest("GET", "/paid", nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("PAYMENT-SIGNATURE", sig)
	blind.ServeHTTP(r3BrokenConn{httptest.NewRecorder()}, req) // settled, write lost
	after := strings.Count(logs.String(), r3BlindMsg)
	t.Logf("blind wrapper with a lost write after settle: new warnings=%d", after-before)
	assert.Equal(t, 1, after, "exactly one warning, and it must be about the blind wrapper")
	assert.Equal(t, int32(1), f.SettleCalls.Load())
}

// The unpaid-clone check for an empty FullPath ("//paid") must not log the
// blind-flush warning: nothing was paid and its writer is the adapter's own.
func TestR3PaidSpellingProbeDoesNotWarn(t *testing.T) {
	if os.Getenv("R3_CHILD") == "" {
		out, ok := r3InChild(t, "TestR3PaidSpellingProbeDoesNotWarn")
		t.Log(out)
		assert.True(t, ok, "child failed")
		return
	}
	logs := &r3SafeBuffer{}
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f, func(c *canton.Config) { c.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { c.String(200, "paid content") })
	w := testkit.Do(r, "GET", "//paid", "")
	t.Logf("unpaid //paid: code=%d logs=%q", w.Code, logs.String())
	assert.NotContains(t, logs.String(), r3BlindMsg)
	_ = http.StatusOK
}
