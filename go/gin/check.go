package gin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"time"
	"unsafe"

	gingonic "github.com/gin-gonic/gin"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/paidpath"
)

// Check is a startup sanity check for the registration traps gin leaves open.
// The runtime guard is what protects: a request that reaches a param or
// catch-all route or NoRoute with a method that is not a paid key's, on a paid
// path in any spelling, is answered 404 by the adapter and no handler runs. Check
// reports the setups where that guard is not in place or a handler of the key is
// registered for the wrong methods. Call it once, after all routes are registered
// and before serving:
//
//	if err := cantongin.Check(r, cfg.Routes); err != nil { log.Fatal(err) }
//
// For each paid key it sends one unpaid in-process request and requires the 402
// offer (a route or group created before r.Use(Middleware(...)) has no gate; a
// key served only by NoRoute or a param route has none either). In a correct
// setup that request never reaches a handler. For each paid path it fails when a
// handler of the key (the same HandlerFunc, from r.Any, r.Match or a second
// registration) is also registered for another method, and sends two more GETs,
// for the "//path" and "/dir//name" spellings, which fail on a 2xx with a body: it means NoRoute
// (a file server, a proxy) is not behind a gate registered with r.Use. Nothing
// else is sent: a free handler of its own on the path (an ingest, a CORS
// preflight) is not run and not judged. A route is judged by its last handler
// (gin's RouteInfo has no more): a terminal handler shared by a paid key and
// another method's chain on the same path (a renderer) is reported. Each request
// has a time bound.
func Check(engine *gingonic.Engine, routes map[string]canton.Route) error {
	var problems []error
	keyed := map[string]map[string]bool{} // path -> keyed methods
	for key := range routes {
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
	// Read the routes before the first request: gin updates its route trees once, in
	// the first ServeHTTP, and a request abandoned on a timeout may still run.
	registered := engine.Routes()
	for key := range routes {
		method, path, found := strings.Cut(key, " ")
		if !found {
			continue
		}
		code, panicked := serve(engine, method, path)
		if panicked != nil {
			problems = append(problems, fmt.Errorf("paid route %q: the unpaid request reached a handler that panicked (%v): the gate is not in front of it", key, panicked))
		} else if code != http.StatusPaymentRequired {
			problems = append(problems, fmt.Errorf(
				"paid route %q answered an unpaid request with %d, not 402: register the gate with r.Use before creating routes and groups, and give the key its own gin route",
				key, code))
		}
	}
	problems = append(problems, otherMethodProblems(engine, registered, keyed)...)
	return errors.Join(problems...)
}

// notifyRecorder is a recorder that implements http.CloseNotifier, as a real
// connection does: gin's CloseNotify asserts it. The channel closes when Check
// gives up on the request, so a streaming handler ends.
type notifyRecorder struct {
	*httptest.ResponseRecorder
	gone chan bool
}

func (r notifyRecorder) CloseNotify() <-chan bool { return r.gone }

// checkTimeout bounds each in-process request Check sends.
var checkTimeout = 5 * time.Second

func serve(engine *gingonic.Engine, method, path string) (code int, panicked any) {
	code, _, panicked = serveBody(engine, method, path)
	return code, panicked
}

func serveBody(engine *gingonic.Engine, method, path string) (code, bodyBytes int, panicked any) {
	ctx, cancel := context.WithTimeout(paidpath.MarkSynthetic(context.Background()), checkTimeout)
	defer cancel()
	recorder := notifyRecorder{ResponseRecorder: httptest.NewRecorder(), gone: make(chan bool)}
	type result struct {
		code, bodyBytes int
		panicked        any
	}
	done := make(chan result, 1)
	go func() {
		var res result
		defer func() { res.panicked = recover(); done <- res }()
		engine.ServeHTTP(recorder, httptest.NewRequestWithContext(ctx, method, path, nil))
		res.code, res.bodyBytes = recorder.Code, recorder.Body.Len()
	}()
	select {
	case res := <-done:
		return res.code, res.bodyBytes, res.panicked
	case <-ctx.Done():
		close(recorder.gone)
		return 0, 0, fmt.Sprintf("no answer within %s (a handler that streams or blocks)", checkTimeout)
	}
}

// otherMethodProblems finds paid paths a method that is not a key can reach
// through a handler of the key, and a NoRoute that no gate is in front of.
func otherMethodProblems(engine *gingonic.Engine, routes gingonic.RoutesInfo, keyed map[string]map[string]bool) []error {
	var problems []error
	paths := make([]string, 0, len(keyed))
	for path := range keyed {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		// The key's own handlers: every route that serves a key's method for this
		// path, a catch-all or param route included (r.Any("/files/*filepath")).
		var keyHandlers []gingonic.RouteInfo
		for _, route := range routes {
			if keyed[path][route.Method] && paidpath.Matches(route.Path, path) {
				keyHandlers = append(keyHandlers, route)
			}
		}
		for _, route := range routes {
			if !paidpath.Matches(route.Path, path) || keyed[path][route.Method] ||
				(route.Method == http.MethodHead && keyed[path][http.MethodGet]) {
				continue
			}
			if sameHandler(keyHandlers, route) {
				problems = append(problems, fmt.Errorf(
					"%s %s reaches the paid handler without payment (not a paid key): r.Any, r.Match or a second registration serves it free",
					route.Method, path))
			}
		}
		// One safe request through NoRoute: a spelling of the paid path. With the
		// gate on r.Use the adapter answers 404 itself; a gate on a group or a route
		// is not in front of NoRoute, which then serves the path by its spelling.
		for _, spelling := range spellings(path) {
			if code, body, panicked := serveBody(engine, http.MethodGet, spelling); panicked != nil || (code >= 200 && code < 300 && body > 0) {
				problems = append(problems, fmt.Errorf(
					"GET %s is answered without payment: NoRoute (a file server, a proxy) serves the path and no gate is in front of it; register the gate with r.Use", spelling))
			}
		}
	}
	return problems
}

// sameHandler reports whether route runs one of the key's handlers. A handler
// name is not an identity: closures made at one call site (gin.WrapH in a loop)
// share a name, and inlining gives one function's closures different names. The
// func value pointer is exact for r.Any, r.Match and one HandlerFunc registered
// twice. A name counts only for a named function, never for a closure name.
// One net/http handler wrapped by two separate gin.WrapH calls is not detected.
func sameHandler(keys []gingonic.RouteInfo, route gingonic.RouteInfo) bool {
	for _, key := range keys {
		if funcPointer(key.HandlerFunc) == funcPointer(route.HandlerFunc) {
			return true
		}
		if key.Handler == route.Handler && !closureName.MatchString(route.Handler) {
			return true
		}
	}
	return false
}

var closureName = regexp.MustCompile(`\.func\d+(\.\d+)*$`)

// funcPointer is the code and closure pointer of a HandlerFunc value.
func funcPointer(h gingonic.HandlerFunc) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&h))
}

// spellings are the two GETs Check sends per paid path: a leading double slash
// (a root NoRoute) and a double slash before the last segment (a NoRoute that
// serves one directory by prefix).
func spellings(path string) []string {
	out := []string{"/" + path}
	if i := strings.LastIndex(path, "/"); i > 0 {
		out = append(out, path[:i]+"/"+path[i:])
	}
	return out
}
