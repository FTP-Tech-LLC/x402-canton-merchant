// Package paidpath is what the gin and echo adapters share to keep a paid path
// away from handlers that do not know it is paid: it asks the gate itself, with
// an unpaid copy of the request, which methods it would charge.
package paidpath

import (
	"context"
	"net/http"
	"strings"
)

// keyMethods are the methods a paid key may have.
var keyMethods = []string{
	http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
}

// Gates reports whether the gate would answer an unpaid request for r's path
// with this method itself (402 or its own refusal) instead of passing it on.
func Gates(gate func(http.Handler) http.Handler, r *http.Request, method string) bool {
	// TRADE-OFF: a shallow copy with empty headers, not r.Clone: the decision
	// depends on the method and the path only, and this runs on free requests.
	probe := *r
	probe = *probe.WithContext(MarkSynthetic(r.Context()))
	probe.Method = method
	probe.Header = http.Header{}
	probe.Body = http.NoBody
	passed := false
	gate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { passed = true })).ServeHTTP(discard{header: http.Header{}}, &probe)
	return !passed
}

type syntheticKey struct{}

// MarkSynthetic marks a context as belonging to a request the adapters made up (a
// guard probe, a startup Check): it says nothing about the real connection.
func MarkSynthetic(ctx context.Context) context.Context {
	return context.WithValue(ctx, syntheticKey{}, true)
}

// Synthetic reports whether ctx belongs to a made-up request.
func Synthetic(ctx context.Context) bool { return ctx.Value(syntheticKey{}) != nil }

// Probe asks a canton gate, through the request context, which key methods it
// charges or refuses for a path. The gate answers from its own index and
// returns without running the SDK or building an offer.
type Probe struct {
	Answered bool
	Methods  map[string]bool
}

type probeKey struct{}

// ProbeFrom returns the probe a request carries, or nil.
func ProbeFrom(ctx context.Context) *Probe {
	p, _ := ctx.Value(probeKey{}).(*Probe)
	return p
}

// KeyMethods are the methods a paid key may have.
func KeyMethods() []string { return keyMethods }

// ask sends one probe through the gate. ok is false when nothing answered it
// (not a canton gate): the caller then runs the gate once per method.
func ask(gate func(http.Handler) http.Handler, r *http.Request) (map[string]bool, bool) {
	p := &Probe{}
	probe := r.WithContext(context.WithValue(MarkSynthetic(r.Context()), probeKey{}, p))
	probe.Header = http.Header{}
	probe.Body = http.NoBody
	gate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(discard{header: http.Header{}}, probe)
	return p.Methods, p.Answered
}

func otherOf(methods map[string]bool, own string) bool {
	for method := range methods {
		if method != own && (own != http.MethodHead || method != http.MethodGet) {
			return true
		}
	}
	return false
}

// OtherMethodGated reports whether the path of r, in the spelling the gate
// sees, is a paid key's path for a method other than r's own. A HEAD follows
// its GET key: it is not another method.
func OtherMethodGated(gate func(http.Handler) http.Handler, r *http.Request) bool {
	if methods, ok := ask(gate, r); ok {
		return otherOf(methods, r.Method)
	}
	for _, method := range keyMethods {
		if method == r.Method || (r.Method == http.MethodHead && method == http.MethodGet) {
			continue
		}
		if Gates(gate, r, method) {
			return true
		}
	}
	return false
}

// AnyMethodGated is OtherMethodGated plus r's own method: for a request no
// route of the framework matched, where nothing may serve a paid path.
func AnyMethodGated(gate func(http.Handler) http.Handler, r *http.Request) bool {
	if methods, ok := ask(gate, r); ok {
		return len(methods) > 0
	}
	return Gates(gate, r, r.Method) || OtherMethodGated(gate, r)
}

type discard struct{ header http.Header }

func (d discard) Header() http.Header       { return d.header }
func (discard) Write(b []byte) (int, error) { return len(b), nil }
func (discard) WriteHeader(int)             {}

// Wildcard reports whether a route pattern has a param or catch-all segment
// (gin ":id", "*rest"; echo ":id", "*").
func Wildcard(pattern string) bool { return strings.ContainsAny(pattern, ":*") }

// Matches reports whether a route pattern (:param, *catch-all) matches a path.
func Matches(pattern, path string) bool {
	patternParts, pathParts := strings.Split(pattern, "/"), strings.Split(path, "/")
	for i, part := range patternParts {
		switch {
		case strings.HasPrefix(part, "*"):
			return true
		case i >= len(pathParts):
			return false
		case strings.HasPrefix(part, ":"):
			if pathParts[i] == "" {
				return false
			}
		case part != pathParts[i]:
			return false
		}
	}
	return len(patternParts) == len(pathParts)
}
