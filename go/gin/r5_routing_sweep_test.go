package gin_test

// Round-5 audit sweep (lens: gin): the gate on r.Use, paid keys served by a
// catch-all route, a param route and r.Static, a NoRoute file server, and gin's
// routing options (RemoveExtraSlash, RedirectFixedPath, UseRawPath with and
// without UnescapePathValues, HandleMethodNotAllowed with a NoMethod file
// server). Every method x spelling over a real HTTP/1.1 server must neither
// return paid bytes unpaid nor settle.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func TestR5RoutingOptionsSweep(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "files"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "assets"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "files", "report.pdf"), []byte("PAID-REPORT"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "assets", "premium.pdf"), []byte("PAID-ASSET"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("free-js"), 0o600))
	routes := map[string]canton.Route{
		"GET /files/report.pdf":   {PayTo: testkit.PayTo, Price: canton.CC("0.01")},
		"GET /v1/data":            {PayTo: testkit.PayTo, Price: canton.CC("0.01")},
		"GET /assets/premium.pdf": {PayTo: testkit.PayTo, Price: canton.CC("0.01")},
	}
	options := map[string]func(*gingonic.Engine){
		"default":                 func(*gingonic.Engine) {},
		"RemoveExtraSlash":        func(e *gingonic.Engine) { e.RemoveExtraSlash = true },
		"RedirectFixedPath":       func(e *gingonic.Engine) { e.RedirectFixedPath = true },
		"UseRawPath+Unescape":     func(e *gingonic.Engine) { e.UseRawPath = true; e.UnescapePathValues = true },
		"UseRawPath, no unescape": func(e *gingonic.Engine) { e.UseRawPath = true; e.UnescapePathValues = false },
		"HandleMethodNotAllowed":  func(e *gingonic.Engine) { e.HandleMethodNotAllowed = true },
	}
	methods := []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "FOO", "TRACE", "get"}
	targets := []string{}
	for _, p := range []string{"/files/report.pdf", "/v1/data", "/assets/premium.pdf"} {
		dirPart, base := p[:strings.LastIndex(p, "/")], p[strings.LastIndex(p, "/")+1:]
		targets = append(targets, p, "/"+p, dirPart+"/"+p[len(dirPart):], dirPart+"/."+p[len(dirPart):],
			dirPart+"/x/.."+p[len(dirPart):], p+"/", strings.ToUpper(p), dirPart+"/%"+hexOf(base[0])+base[1:],
			p+"%2F", p+";x", "/."+p, strings.Replace(p[1:], "/", "%2F", 1), "/"+strings.Replace(p[1:], "/", "%2F", 1),
			dirPart+"/"+strings.ToUpper(base), p+"/.", p+"%00", p+"%20", p+".")
	}
	for name, option := range options {
		f := testkit.StartFacilitator(t)
		g := testkit.NewGate(t, f, func(c *canton.Config) { c.Routes = routes })
		r := gingonic.New()
		option(r)
		r.Use(gingonic.Recovery())
		r.Use(cantongin.Middleware(g.Middleware()))
		r.Any("/files/*filepath", gingonic.WrapH(http.FileServer(http.Dir(dir))))
		r.Match([]string{"GET", "POST", "PUT", "PATCH", "DELETE"}, "/v1/:name", func(c *gingonic.Context) {
			if c.Param("name") == "data" {
				c.String(200, "PAID-DATA")
				return
			}
			c.String(200, "free dataset %s", c.Param("name"))
		})
		r.Static("/assets", filepath.Join(dir, "assets"))
		fileServer := gingonic.WrapH(http.FileServer(http.Dir(dir)))
		r.NoRoute(fileServer)
		r.NoMethod(fileServer)
		checkErr := cantongin.Check(r, routes)
		srv := httptest.NewServer(r)
		addr := strings.TrimPrefix(srv.URL, "http://")
		leaks, total := 0, 0
		for _, method := range methods {
			for _, target := range targets {
				code, body := r4RawRequest(t, addr, method, target)
				total++
				if strings.Contains(body, "PAID-") {
					leaks++
					t.Logf("%s: %s %s: %d %q", name, method, target, code, body)
				}
			}
		}
		// Free content keeps working: a static asset and a free dataset.
		freeJS, _ := r4RawRequest(t, addr, "GET", "/assets/app.js")
		freeData, freeBody := r4RawRequest(t, addr, "POST", "/v1/other")
		srv.Close()
		t.Logf("%s: Check=%v; %d requests, %d leaks, settles=%d; GET /assets/app.js=%d, POST /v1/other=%d %q",
			name, checkErr, total, leaks, f.SettleCalls.Load(), freeJS, freeData, freeBody)
		assert.Zero(t, leaks, name)
		assert.Zero(t, f.SettleCalls.Load(), name)
		assert.Equal(t, 200, freeJS, name)
		assert.Equal(t, 200, freeData, name)
	}
}

func hexOf(b byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[b>>4], digits[b&15]})
}
