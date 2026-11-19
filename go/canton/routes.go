package canton

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	x402 "github.com/x402-foundation/x402/go/v2"
	x402http "github.com/x402-foundation/x402/go/v2/http"

	cantonmech "github.com/FTP-Tech-LLC/x402-canton-merchant/go/mechanisms/canton"
)

var (
	partyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*::1220[0-9a-f]{64}$`)
	// allowedMethods are the verbs a paid route may name. HEAD is gated through
	// the GET route by the SDK, so it is not a key of its own.
	allowedMethods = map[string]bool{
		http.MethodGet: true, http.MethodPost: true, http.MethodPut: true,
		http.MethodPatch: true, http.MethodDelete: true,
	}
	// forbiddenPathBytes are pattern syntax of the SDK (*, :x, [x]) and URL
	// syntax (?, #); a literal path has none of them. % is refused too: the
	// SDK matches the decoded path, so an escaped key would never match.
	forbiddenPathBytes = "*:[]{}$?#% \t\r\n\\"
)

// checkedRoute is a Route that passed validation, with defaults applied.
type checkedRoute struct {
	key                  string
	method               string
	path                 string
	route                Route
	executeBeforeSeconds int
}

// checkRoutes validates every route key and field. All problems are returned
// together so a merchant fixes the config in one pass.
func checkRoutes(routes map[string]Route) ([]checkedRoute, []error) {
	if len(routes) == 0 {
		return nil, []error{errors.New("routes is empty: configure at least one paid route")}
	}
	keys := make([]string, 0, len(routes))
	for key := range routes {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var problems []error
	var checked []checkedRoute
	seen := make(map[string]string) // loose key -> route key
	for _, key := range keys {
		route, err := checkRoute(key, routes[key])
		if err != nil {
			problems = append(problems, fmt.Errorf("route %q: %w", key, err))
			continue
		}
		if first, duplicate := seen[looseKey(route.method, route.path)]; duplicate {
			problems = append(problems, fmt.Errorf("route %q duplicates route %q (paths are compared case-insensitively)", key, first))
			continue
		}
		seen[looseKey(route.method, route.path)] = key
		checked = append(checked, route)
	}
	return checked, problems
}

func checkRoute(key string, route Route) (checkedRoute, error) {
	method, urlPath, err := splitRouteKey(key)
	if err != nil {
		return checkedRoute{}, err
	}
	checked := checkedRoute{key: key, method: method, path: urlPath, route: route, executeBeforeSeconds: route.ExecuteBeforeSeconds}
	if checked.executeBeforeSeconds == 0 {
		checked.executeBeforeSeconds = defaultExecuteBefore
	}
	return checked, errors.Join(
		checkPayTo(route.PayTo),
		route.Price.validate(),
		checkExecuteBefore(checked.executeBeforeSeconds),
		checkMemo(route.Memo),
	)
}

func splitRouteKey(key string) (method, urlPath string, err error) {
	method, urlPath, found := strings.Cut(key, " ")
	if !found || strings.ContainsAny(urlPath, " \t") || strings.TrimSpace(key) != key {
		return "", "", errors.New(`key must be "METHOD /path" with one space`)
	}
	if !allowedMethods[method] {
		return "", "", fmt.Errorf("method %q must be one of GET, POST, PUT, PATCH, DELETE", method)
	}
	if err := checkLiteralPath(urlPath); err != nil {
		return "", "", err
	}
	return method, urlPath, nil
}

func checkLiteralPath(urlPath string) error {
	if !strings.HasPrefix(urlPath, "/") {
		return fmt.Errorf("path %q must start with /", urlPath)
	}
	if strings.ContainsAny(urlPath, "{}$") {
		return fmt.Errorf("path %q uses a ServeMux wildcard ({name}, {name...}, {$}), which is not supported: list each literal path", urlPath)
	}
	if urlPath == "/" {
		return errors.New(`a paid root "/" is not supported: ServeMux "GET /" is a catch-all that would serve every other path free; price a literal path`)
	}
	if strings.ContainsAny(urlPath, forbiddenPathBytes) {
		return fmt.Errorf("path %q must be literal: no * :x [x] ? # %% or spaces", urlPath)
	}
	for _, r := range urlPath {
		if r < ' ' || r > '~' {
			return fmt.Errorf("path %q must be printable ASCII", urlPath)
		}
	}
	// The SDK strips a trailing slash from the request path but not from the
	// key, so "/paid/" would silently never match; dot segments and repeated
	// slashes never match either.
	if strings.HasSuffix(urlPath, "/") || strings.Contains(urlPath, "//") || hasDotSegment(urlPath) {
		return fmt.Errorf("path %q must be canonical: no trailing slash, //, or dot segments", urlPath)
	}
	return nil
}

func hasDotSegment(urlPath string) bool {
	for _, segment := range strings.Split(urlPath, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

func checkPayTo(payTo string) error {
	if !partyPattern.MatchString(payTo) {
		return fmt.Errorf("PayTo %q must look like <hint>::1220<64 hex>", payTo)
	}
	return nil
}

func checkExecuteBefore(seconds int) error {
	if seconds < minExecuteBefore || seconds > maxExecuteBefore {
		return fmt.Errorf("ExecuteBeforeSeconds %d must be %d..%d", seconds, minExecuteBefore, maxExecuteBefore)
	}
	return nil
}

func checkMemo(memo string) error {
	if memo == "" {
		return nil
	}
	if len(memo) > maxMemoBytes || !utf8.ValidString(memo) {
		return fmt.Errorf("memo must be valid UTF-8 of at most %d bytes", maxMemoBytes)
	}
	if strings.TrimSpace(memo) != memo {
		return errors.New("memo must have no leading or trailing whitespace")
	}
	return nil
}

// checkTokens refuses a token the facilitator does not list. An instrument the
// facilitator lists with an id must match on admin and id; one listed by admin
// alone matches on admin. When /supported lists no instruments at all, the check
// is skipped.
func checkTokens(routes []checkedRoute, supported supportedInstruments) []error {
	if len(supported.admins) == 0 {
		return nil
	}
	var problems []error
	for _, route := range routes {
		price := route.route.Price
		if price.isCantonCoin() {
			continue
		}
		admin, id := price.instrument.Admin, price.instrument.ID
		switch {
		case !supported.admins[admin]:
			problems = append(problems, fmt.Errorf("route %q: token admin %q is not in the facilitator's /supported instruments", route.key, admin))
		case !supported.matches(admin, id):
			problems = append(problems, fmt.Errorf("route %q: token id %q is not listed for admin %q in /supported", route.key, id, admin))
		}
	}
	return problems
}

// sdkRoutes builds the SDK route table. Every route is authorization flow: no
// paymentFlow is set, so the mirror scheme's default (settle after the
// handler) applies and upfront is never offered.
func sdkRoutes(routes []checkedRoute) x402http.RoutesConfig {
	out := make(x402http.RoutesConfig, len(routes))
	for _, route := range routes {
		extra := map[string]interface{}{
			"instrumentId": map[string]interface{}{
				"admin": route.route.Price.instrument.Admin,
				"id":    route.route.Price.instrument.ID,
			},
			"executeBeforeSeconds": route.executeBeforeSeconds,
		}
		if route.route.Memo != "" {
			extra["memo"] = route.route.Memo
		}
		out[route.key] = x402http.RouteConfig{
			Accepts: x402http.PaymentOptions{{
				Scheme:            cantonmech.SchemeExact,
				Network:           x402.Network(cantonmech.CantonMainnetCAIP2),
				PayTo:             route.route.PayTo,
				MaxTimeoutSeconds: route.executeBeforeSeconds,
				Price:             x402.AssetAmount{Amount: route.route.Price.atomic, Asset: route.route.Price.asset(), Extra: extra},
			}},
			Description: route.route.Description,
			MimeType:    route.route.MimeType,
		}
	}
	return out
}

func newLooseIndex(routes []checkedRoute) looseRouteIndex {
	index := make(looseRouteIndex, len(routes))
	for _, route := range routes {
		index[looseKey(route.method, route.path)] = route.executeBeforeSeconds
	}
	return index
}
