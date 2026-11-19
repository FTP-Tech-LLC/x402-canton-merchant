package canton

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	x402 "github.com/x402-foundation/x402/go/v2"
	x402http "github.com/x402-foundation/x402/go/v2/http"
	"github.com/x402-foundation/x402/go/v2/http/nethttp"
	"github.com/x402-foundation/x402/go/v2/types"

	cantonmech "github.com/FTP-Tech-LLC/x402-canton-merchant/go/mechanisms/canton"
)

const (
	supportedVersion       = 2
	memoryStoreWarn        = "memory store: one process only; use goredis.NewStore (or NewRedisStore) for more than one instance and to keep deliveries across a restart"
	memoryStoreRebuildWarn = "memory store: it belongs to this gate. A gate that replaces another (a config reload) must be given the same Config.Store, or a replayed payment can be delivered again"
)

// NewMiddleware builds the paid-route gate for net/http. It never exits the
// process: every startup problem comes back as an error. It retries GET
// /supported for up to SupportedWait, requires a canton:mainnet exact kind with
// feePayer and synchronizerId, and runs the SDK's initialization itself, so the
// SDK's fatal startup path (os.Exit) cannot fire later.
//
//	mw, err := canton.NewMiddleware(ctx, canton.Config{Routes: ...})
//	http.ListenAndServe(addr, mw(mux))
func NewMiddleware(ctx context.Context, cfg Config) (func(http.Handler) http.Handler, error) {
	g, err := buildGate(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		g.logger.Warn("the context passed to NewMiddleware has a deadline: the background reconciler stops when it ends, " +
			"and claims, locks and records left by a Store outage are then no longer released. " +
			"Pass a context that lives as long as the server (startup is already bounded by SupportedWait), or use NewGate and Close")
	}
	// The reconciler is bound to ctx. NewGate gives it its own lifetime instead.
	go g.ledger.run(ctx, reconcileInterval, true)
	return g.wrap, nil
}

// Gate is a paid-route gate with its own lifetime. It owns one background
// reconciler, which retries what a Store outage left behind (claims, locks and
// records held only in this process) until Close.
//
// Call Close after http.Server.Shutdown has returned: requests that were in
// flight during the grace period may have left work behind, and Close runs one
// last bounded pass for it. Close waits for this gate's paid requests still in
// flight (and their post-response work) while the reconciler keeps running, so
// pass a ctx of at least RequestBudget. On a reload, swap the handler first and
// then Close the old gate. Each NewGate or NewMiddleware call starts one
// goroutine and keeps its ledger (and a default memory store) alive: build the
// gate once per process, or Close the old one when a config reload replaces
// it, and hand the new one the same Config.Store (a default memory store is
// not shared, so a rebuilt gate would deliver a replayed payment again).
type Gate struct {
	g      *gate
	stop   context.CancelFunc
	done   chan struct{}
	closed sync.Once
	ended  chan struct{} // closed when the close work is over
}

// NewGate builds the gate like NewMiddleware, but the reconciler lives until
// Close, whatever happens to ctx (a startup context with a timeout is fine).
func NewGate(ctx context.Context, cfg Config) (*Gate, error) {
	g, err := buildGate(ctx, cfg)
	if err != nil {
		return nil, err
	}
	runCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	gate := &Gate{g: g, stop: stop, done: make(chan struct{}), ended: make(chan struct{})}
	interval := reconcileInterval
	go func() {
		defer close(gate.done)
		g.ledger.run(runCtx, interval, false)
	}()
	return gate, nil
}

// Middleware returns the net/http middleware: wrap your handler with it.
func (g *Gate) Middleware() func(http.Handler) http.Handler { return g.g.wrap }

// Close ends the gate's life. From the moment it is called, a paid request that
// carries a payment is refused with 503 "payment in progress: retry the same
// payment" (the unpaid 402 offer is still answered): nothing would reconcile what
// it leaves behind, and the retry reaches the new gate. Close then waits for the
// paid requests already running and their post-response work, with the reconciler
// still running for what they leave, stops the reconciler, runs one last pass
// (bounded to 30 s) and logs at error what is left. The work runs once, on its own
// goroutine; every caller waits up to its own ctx, so a ctx that ends first only
// stops that caller's wait, and the work goes on. Pass a ctx of at least
// RequestBudget; on a reload swap the handler first. Safe to call more than once.
func (g *Gate) Close(ctx context.Context) error {
	g.closed.Do(func() { go g.closeWork() }) //nolint:contextcheck // the close work outlives any one caller's ctx by design
	select {
	case <-g.ended:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// finalPassBound bounds Close's last reconcile pass.
const finalPassBound = 30 * time.Second

func (g *Gate) closeWork() {
	defer close(g.ended)
	<-g.g.ledger.beginClose()
	g.stop()
	<-g.done
	ctx, cancel := context.WithTimeout(context.Background(), finalPassBound)
	defer cancel()
	g.g.ledger.reconcileGuarded(ctx)
	g.g.ledger.logLeftBehind()
}

// buildGate does the work of NewMiddleware and returns the gate itself, so
// tests can reach the SDK server behind it.
func buildGate(ctx context.Context, cfg Config) (*gate, error) {
	conf, err := cfg.resolve()
	if err != nil {
		return nil, fmt.Errorf("canton: invalid config: %w", err)
	}
	logStartupWarnings(conf)

	// The copy has no overall Timeout of its own: every call is bounded by its
	// context (Timeout for verify, SettleTimeout per /settle), and a client
	// Timeout below SettleTimeout would cut a settle the facilitator is committing.
	httpClient := *conf.HTTPClient
	httpClient.Timeout = 0
	httpClient.Transport = answerRecorder{base: transportFor(conf, conf.HTTPClient.Transport)}
	sdkClient := x402http.NewHTTPFacilitatorClient(&x402http.FacilitatorConfig{URL: conf.FacilitatorURL, HTTPClient: &httpClient})
	client := &facilitatorClient{inner: sdkClient, settleTimeout: conf.SettleTimeout, settleCalls: conf.SettleCalls, logger: conf.Logger}
	led := newLedger(conf.Store, conf.Logger)

	httpServer, err := startServer(ctx, conf, client, led)
	if err != nil {
		return nil, err
	}
	g := &gate{
		httpServer:       httpServer,
		ledger:           led,
		logger:           conf.Logger,
		routes:           newLooseIndex(conf.routes),
		timeout:          conf.Timeout,
		settleTimeout:    conf.SettleTimeout,
		settleCalls:      conf.SettleCalls,
		settleOnRedirect: conf.SettleOnRedirect,
	}
	g.sdk = nethttp.PaymentMiddlewareFromHTTPServer(httpServer,
		nethttp.WithSyncFacilitatorOnStart(false), // initialized above: errors are values
		nethttp.WithTimeout(conf.Timeout),
		nethttp.WithErrorHandler(g.onSettleFailed),
		nethttp.WithSettlementHandler(g.onSettled),
	)
	return g, nil
}

// reconcileInterval is how often the background reconciler retries. A variable
// so tests can run fast.
var reconcileInterval = 2 * time.Second

// supportedRetryPause is the wait between startup attempts. A variable so
// tests can run fast.
var supportedRetryPause = time.Second

// permanentError marks a startup problem that retrying cannot fix.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// startServer reads /supported and runs the SDK's initialization, both inside
// one retry loop of SupportedWait with a timeout per attempt. The SDK never
// re-reads /supported after a failed start, so a facilitator that is briefly
// down or answering 502 at boot is absorbed here.
func startServer(ctx context.Context, conf resolved, client x402.FacilitatorClient, led *ledger) (*x402http.HTTPServer, error) {
	deadline := time.Now().Add(conf.SupportedWait)
	for attempt := 1; ; attempt++ {
		httpServer, err := tryStart(ctx, conf, client, led, deadline)
		if err == nil {
			return httpServer, nil
		}
		var permanent permanentError
		if errors.As(err, &permanent) {
			return nil, fmt.Errorf("canton: %w", err)
		}
		conf.Logger.Warn("facilitator /supported not usable yet", slog.Int("attempt", attempt), slog.String("error", err.Error()))
		if time.Now().Add(supportedRetryPause).After(deadline) {
			var store storeUnusable
			if errors.As(err, &store) {
				return nil, fmt.Errorf("canton: payment store unusable after %s: %w", conf.SupportedWait, store.error)
			}
			return nil, fmt.Errorf("canton: facilitator /supported unusable after %s: %w", conf.SupportedWait, err)
		}
		select {
		case <-time.After(supportedRetryPause):
		case <-ctx.Done():
			return nil, fmt.Errorf("canton: waiting for /supported: %w", ctx.Err())
		}
	}
}

// storeUnusable marks a startup failure caused by the Store, so the final error
// names the Store and not the facilitator.
type storeUnusable struct{ error }

func (e storeUnusable) Unwrap() error { return e.error }

func tryStart(ctx context.Context, conf resolved, client x402.FacilitatorClient, led *ledger, deadline time.Time) (*x402http.HTTPServer, error) {
	// One /supported read per attempt: the SDK is given the answer that was
	// validated, so a glitch cannot pass our check and then fail the SDK's own.
	attemptCtx, cancel := context.WithTimeout(ctx, min(supportedAttemptTimeout, max(time.Until(deadline), time.Millisecond)))
	defer cancel()
	response, kind, err := fetchKind(attemptCtx, client)
	if err != nil {
		return nil, err
	}
	if problems := checkTokens(conf.routes, listedInstruments(kind)); len(problems) > 0 {
		return nil, permanentError{errors.Join(problems...)}
	}
	if err := led.probe(attemptCtx); err != nil {
		if errors.Is(err, errNilRedisClient) {
			return nil, permanentError{err} // no retry can fix a nil client
		}
		var clusterErr errClusterEndpoint
		if errors.As(err, &clusterErr) {
			return nil, permanentError{fmt.Errorf("payment store not usable: %w", err)} // no retry fixes a wiring mistake
		}
		return nil, storeUnusable{fmt.Errorf("payment store not usable: %w", err)}
	}
	resourceServer := x402.Newx402ResourceServer(x402.WithFacilitatorClient(pinnedSupported{FacilitatorClient: client, supported: response}))
	resourceServer.Register(x402.Network(cantonmech.CantonMainnetCAIP2), newFTPScheme(led, conf.Logger))
	httpServer := x402http.Wrappedx402HTTPResourceServer(sdkRoutes(conf.routes), resourceServer)
	httpServer.RegisterPaywallProvider(cantonPaywall{})
	if err := httpServer.Initialize(attemptCtx); err != nil {
		if x402http.IsFatalStartupInitError(err) {
			return nil, permanentError{fmt.Errorf("x402 initialization failed: %w", err)}
		}
		return nil, err
	}
	return httpServer, nil
}

// pinnedSupported answers GetSupported with the response already validated.
type pinnedSupported struct {
	x402.FacilitatorClient
	supported x402.SupportedResponse
}

func (p pinnedSupported) GetSupported(context.Context) (x402.SupportedResponse, error) {
	return p.supported, nil
}

// transportFor returns the transport the facilitator calls use. A merchant
// *http.Transport with a ResponseHeaderTimeout below SettleTimeout would cut a
// slow /settle short, so it is cloned with that timeout lifted: every call is
// bounded by its own context. A transport set up with x/net/http2 keeps its
// timeout in the h2 connections, so the clone drops that hook and uses the
// bundled HTTP/2 instead. A wrapped RoundTripper cannot be lifted: it is
// warned about, and OPERATIONS.md says its timeouts must be 0 or at least
// SettleTimeout.
func transportFor(conf resolved, base http.RoundTripper) http.RoundTripper {
	transport, ok := base.(*http.Transport)
	if !ok {
		if base != nil {
			conf.Logger.Warn("HTTPClient transport is wrapped: a ResponseHeaderTimeout inside it is not lifted and must be 0 or at least SettleTimeout",
				slog.Duration("settleTimeout", conf.SettleTimeout))
		}
		return transportOrDefault(base)
	}
	if transport.ResponseHeaderTimeout <= 0 || transport.ResponseHeaderTimeout >= conf.SettleTimeout {
		return transport
	}
	conf.Logger.Info("HTTPClient transport ResponseHeaderTimeout is below SettleTimeout: lifted on a clone, calls are bounded by their own context",
		slog.Duration("responseHeaderTimeout", transport.ResponseHeaderTimeout), slog.Duration("settleTimeout", conf.SettleTimeout))
	clone := transport.Clone()
	clone.ResponseHeaderTimeout = 0
	if _, configuredH2 := clone.TLSNextProto["h2"]; configuredH2 {
		clone.TLSNextProto = nil
		clone.ForceAttemptHTTP2 = true
	}
	return clone
}

func transportOrDefault(transport http.RoundTripper) http.RoundTripper {
	if transport == nil {
		return http.DefaultTransport
	}
	return transport
}

func logStartupWarnings(conf resolved) {
	if _, isMemory := conf.Store.(*memoryStore); isMemory {
		conf.Logger.Warn(memoryStoreWarn)
		conf.Logger.Warn(memoryStoreRebuildWarn)
	}
	if conf.SettleTimeout < defaultSettleTimeout {
		conf.Logger.Warn("SettleTimeout is below the 65 s a slow /settle can take: a slow settle becomes a 503 and a retry",
			slog.Duration("settleTimeout", conf.SettleTimeout))
	}
}

func fetchKind(ctx context.Context, client x402.FacilitatorClient) (x402.SupportedResponse, types.SupportedKind, error) {
	response, err := client.GetSupported(ctx)
	if err != nil {
		return x402.SupportedResponse{}, types.SupportedKind{}, err
	}
	for _, kind := range response.Kinds {
		if kind.X402Version == supportedVersion && kind.Scheme == cantonmech.SchemeExact && kind.Network == cantonmech.CantonMainnetCAIP2 {
			if err := validateKindExtra(kind); err != nil {
				return x402.SupportedResponse{}, types.SupportedKind{}, err
			}
			return response, kind, nil
		}
	}
	return x402.SupportedResponse{}, types.SupportedKind{}, errors.New("facilitator /supported has no x402Version 2 exact canton:mainnet kind")
}

// supportedInstruments are the instruments the facilitator lists.
type supportedInstruments struct {
	admins map[string]bool
	pairs  map[string]bool // "<admin>|<id>" for instruments listed with an id
	bare   map[string]bool // admins listed without an id
}

func (s supportedInstruments) matches(admin, id string) bool {
	return s.bare[admin] || s.pairs[admin+"|"+id]
}

func listedInstruments(kind types.SupportedKind) supportedInstruments {
	out := supportedInstruments{admins: map[string]bool{}, pairs: map[string]bool{}, bare: map[string]bool{}}
	list, _ := kind.Extra["instruments"].([]interface{})
	for _, item := range list {
		entry, _ := item.(map[string]interface{})
		if id, ok := cantonmech.InstrumentIDFromExtra(entry); ok {
			out.admins[id.Admin] = true
			out.pairs[id.Admin+"|"+id.ID] = true
			continue
		}
		if admin, ok := entry["admin"].(string); ok {
			out.admins[admin] = true
			out.bare[admin] = true
		}
	}
	return out
}
