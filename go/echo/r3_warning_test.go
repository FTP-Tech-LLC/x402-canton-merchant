package echo_test

// Round-3 audit probe (cross-check of the core warning finding): echo's Check
// also spends the once-per-process blind-flush warning on a correct setup.

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

type r3Buf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *r3Buf) Write(p []byte) (int, error) { b.mu.Lock(); defer b.mu.Unlock(); return b.b.Write(p) }
func (b *r3Buf) String() string              { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

func TestR3EchoCheckDoesNotSpendTheBlindFlushWarning(t *testing.T) {
	if os.Getenv("R3_CHILD") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestR3EchoCheckDoesNotSpendTheBlindFlushWarning$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), "R3_CHILD=1")
		out, err := cmd.CombinedOutput()
		t.Log(string(out))
		assert.NoError(t, err, "child failed")
		return
	}
	logs := &r3Buf{}
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f, func(c *canton.Config) { c.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	e := echolib.New()
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return c.String(200, "paid content") })
	require.NoError(t, cantonecho.Check(e, testkit.Routes()))
	t.Logf("after echo Check: warnings=%d", strings.Count(logs.String(), "cannot see the connection"))
	assert.NotContains(t, logs.String(), "cannot see the connection")
}
