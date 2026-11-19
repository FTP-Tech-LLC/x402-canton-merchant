package gin_test

// Round-4 audit sweep (lens: gin): the README setup (gate on r.Use) with a
// NoRoute file server and, separately, a NoRoute reverse proxy to a
// method-agnostic upstream. Every method x spelling of the paid path must
// neither return the paid bytes unpaid nor settle.

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func r4RawRequest(t *testing.T, addr, method, target string) (int, string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer conn.Close()
	_, err = fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: x\r\nAccept: application/json\r\nConnection: close\r\nContent-Length: 0\r\n\r\n", method, target)
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: method})
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	var b strings.Builder
	_, _ = bufio.NewReader(resp.Body).WriteTo(&b)
	return resp.StatusCode, b.String()
}

func TestR4SweepGlobalGateNoRoute(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "paid"), []byte("PAID-FILE"), 0o600))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(strings.Trim(filepath.Clean("/"+r.URL.Path), "/"), "paid") {
			_, _ = w.Write([]byte("PAID-UPSTREAM"))
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(upstream.Close)
	target, _ := url.Parse(upstream.URL)
	for name, noRoute := range map[string]http.Handler{
		"fileserver": http.FileServer(http.Dir(dir)),
		"proxy":      httputil.NewSingleHostReverseProxy(target),
	} {
		f := testkit.StartFacilitator(t)
		g := testkit.NewGate(t, f)
		r := gingonic.New()
		r.Use(gingonic.Recovery())
		r.Use(cantongin.Middleware(g.Middleware()))
		r.GET("/paid", func(c *gingonic.Context) { c.String(200, "PAID-ROUTE") })
		r.NoRoute(gingonic.WrapH(noRoute))
		require.NoError(t, cantongin.Check(r, testkit.Routes()), name)
		srv := httptest.NewServer(r)
		addr := strings.TrimPrefix(srv.URL, "http://")
		leaks := 0
		total := 0
		for _, method := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "FOO", "TRACE"} {
			for _, spelling := range []string{"/paid", "//paid", "/./paid", "/x/../paid", "/paid/", "/PAID", "/%70aid", "/paid%2F", "/paid;x", "/paid?x=1", "///paid", "/.//paid", "/paid/.", "/%2fpaid", "/%2Fpaid"} {
				code, body := r4RawRequest(t, addr, method, spelling)
				total++
				if strings.Contains(body, "PAID-") {
					leaks++
					t.Logf("%s %s %s: %d %q", name, method, spelling, code, body)
				}
			}
		}
		srv.Close()
		t.Logf("%s: %d requests, %d leaks, settles=%d", name, total, leaks, f.SettleCalls.Load())
		assert.Zero(t, leaks, name)
		assert.Zero(t, f.SettleCalls.Load(), name)
	}
}
