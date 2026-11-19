package echo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"time"

	echolib "github.com/labstack/echo/v4"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/paidpath"
)

// Check is a startup sanity check for the registration traps echo leaves open.
// The runtime guard is what protects: a request that echo sends to a param or
// catch-all route, RouteNotFound or its own 404/405 (Static inside the gate) with a
// method that is not a paid key's, on a paid path in any spelling, is answered 404
// by the adapter and no handler runs. Check reports setups where the guard is not
// in place or the key's handler is registered for the wrong methods. Call it once,
// after all routes are registered and before serving:
//
//	if err := cantonecho.Check(e, cfg.Routes); err != nil { log.Fatal(err) }
//
// Every paid key needs an exact echo route (same method and path) in at least one
// router: the default one and every e.Host router are scanned. For each such
// router Check sends one unpaid in-process request and requires the 402 offer (a
// group or route registered before the gate has none); in a correct setup that
// request never reaches a handler. For each paid path it fails when a route for
// another method (OPTIONS included) has the key route's handler name (e.Any,
// e.Match, a second registration), and sends two GETs, for the "//path" and
// "/dir//name" spellings, which fail on a 2xx with a body: RouteNotFound or Static serves the path and no
// gate is in front of it. Nothing else is sent: a free handler of its own on the
// path (an ingest, a CORS preflight) is not run and not judged. Handler names
// are echo's only identity: two closures made at one call site look alike, and a
// net/http handler wrapped twice with echo.WrapHandler is not detected (inlining
// changes the names).
// Each request has a time bound.
//
// Check cannot see the gate in e.Pre (it logs an error on the first paid
// request instead); the same gate registered twice passes the request through.
func Check(e *echolib.Echo, routes map[string]canton.Route) error {
	var problems []error
	keyed := map[string]map[string]bool{} // path -> keyed methods
	var keys []string
	for key := range routes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		method, path, found := strings.Cut(key, " ")
		if !found {
			problems = append(problems, fmt.Errorf("paid key %q is not \"METHOD /path\"", key))
			continue
		}
		if keyed[path] == nil {
			keyed[path] = map[string]bool{}
		}
		keyed[path][method] = true
	}

	routers := map[string]*echolib.Router{"": e.Router()}
	for host, router := range e.Routers() {
		routers[host] = router
	}
	for _, key := range keys {
		method, path, found := strings.Cut(key, " ")
		if !found {
			continue
		}
		served := false
		for host, router := range routers {
			if !hasRoute(router, method, path) {
				continue
			}
			served = true
			code, panicked := serve(e, host, method, path)
			if panicked != nil || code != http.StatusPaymentRequired {
				problems = append(problems, fmt.Errorf(
					"paid route %q (host %q) answered an unpaid request with %d, not 402: register the gate with e.Use before creating routes and groups",
					key, host, code))
			}
		}
		if !served {
			problems = append(problems, fmt.Errorf("paid key %q has no echo route with that method and path (a typo, or a param or catch-all route): it would never be delivered", key))
		}
	}
	for host, router := range routers {
		problems = append(problems, otherMethodProblems(e, host, router, keyed)...)
	}
	return errors.Join(problems...)
}

// otherMethodProblems judges one router by its routes, without running a
// handler: a route for a method that is not a key, on a paid path (exact, param
// or catch-all), that has the key route's handler name is the key's handler
// served free (e.Any, e.Match, a second registration). A free handler of its
// own is not judged. Then one GET for the "//path" spelling must not be served
// by RouteNotFound or Static: no gate is in front of them.
func otherMethodProblems(e *echolib.Echo, host string, router *echolib.Router, keyed map[string]map[string]bool) []error {
	var problems []error
	routes := router.Routes()
	paths := make([]string, 0, len(keyed))
	for path := range keyed {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		keyNames := map[string]bool{}
		for _, route := range routes {
			if keyed[path][route.Method] && route.Path == path { // the key's own route: a catch-all fallback is not it
				keyNames[route.Name] = true
			}
		}
		if len(keyNames) == 0 {
			continue // the key has no route in this router
		}
		for _, route := range routes {
			if route.Method == echolib.RouteNotFound || keyed[path][route.Method] ||
				(route.Method == http.MethodHead && keyed[path][http.MethodGet]) || !paidpath.Matches(route.Path, path) {
				continue
			}
			if keyNames[route.Name] {
				problems = append(problems, fmt.Errorf(
					"echo route %s %s (host %q) runs the paid handler without payment (not a paid key): e.Any, e.Match or a second registration serves it free",
					route.Method, path, host))
			}
		}
		for _, spelling := range []string{"/" + path, dirSpelling(path)} {
			if spelling == "" {
				continue
			}
			if code, body, panicked := serveBody(e, host, http.MethodGet, spelling); panicked != nil || (code >= 200 && code < 300 && body > 0) {
				problems = append(problems, fmt.Errorf(
					"GET %s (host %q) is answered without payment: RouteNotFound or Static serves the path and no gate is in front of it; register the gate with e.Use", spelling, host))
			}
		}
	}
	return problems
}

func hasRoute(router *echolib.Router, method, path string) bool {
	for _, route := range router.Routes() {
		if route.Method == method && route.Path == path {
			return true
		}
	}
	return false
}

// checkTimeout bounds each in-process request Check sends.
var checkTimeout = 5 * time.Second

func serve(e *echolib.Echo, host, method, path string) (code int, panicked any) {
	code, _, panicked = serveBody(e, host, method, path)
	return code, panicked
}

func serveBody(e *echolib.Echo, host, method, path string) (code, bodyBytes int, panicked any) {
	ctx, cancel := context.WithTimeout(paidpath.MarkSynthetic(context.Background()), checkTimeout)
	defer cancel()
	request := httptest.NewRequestWithContext(ctx, method, path, nil)
	if host != "" {
		request.Host = host
	}
	recorder := httptest.NewRecorder()
	type result struct {
		code, bodyBytes int
		panicked        any
	}
	done := make(chan result, 1)
	go func() {
		var res result
		defer func() { res.panicked = recover(); done <- res }()
		e.ServeHTTP(recorder, request)
		res.code, res.bodyBytes = recorder.Code, recorder.Body.Len()
	}()
	select {
	case res := <-done:
		return res.code, res.bodyBytes, res.panicked
	case <-ctx.Done():
		return 0, 0, fmt.Sprintf("no answer within %s (a handler that streams or blocks)", checkTimeout)
	}
}

// dirSpelling puts a double slash before the last segment ("/a//b"), the form a
// RouteNotFound or Static that serves one directory by prefix does not clean.
func dirSpelling(path string) string {
	if i := strings.LastIndex(path, "/"); i > 0 {
		return path[:i] + "/" + path[i:]
	}
	return ""
}
