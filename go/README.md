# Canton x402 merchant plugin for Go

Charge per request in Canton Coin or a CIP-56 token on a Go server. It sits in front of the official [x402 Go SDK](https://github.com/x402-foundation/x402) and settles through FTP's facilitator after your handler answers 2xx.

## Install

```bash
go get github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton
```

Needs Go 1.26.10 or newer and `github.com/x402-foundation/x402/go/v2` v2.31.0 or newer. Works with `net/http` (put any router, such as chi or gorilla mux, behind the gate), gin and echo v4. MainNet only: the facilitator serves `canton:mainnet`. The optional `.../go/goredis` package needs `github.com/redis/go-redis/v9`.

## Before you start

The party that receives payments needs a live `TransferPreapproval`:

```bash
npx @ftptech/canton-agent-wallet create --relay-url https://facilitator.ftptech.xyz
npx @ftptech/canton-agent-wallet address       # your party id: payTo
# send it a few CC, then
npx @ftptech/canton-agent-wallet claim
npx @ftptech/canton-agent-wallet preapproval   # lasts 30 days
```

For a token, also run `npx @ftptech/canton-agent-wallet preapproval --admin <registrar> --id <instrument>` once per token. Payments to an expired preapproval fail: run `preapproval` again before then (`--force` renews early, for CC). `~/.canton-agent/wallet.json` holds the key of the party you are paid to: back it up with `npx @ftptech/canton-agent-wallet export`.

## Quickstart

```go
paid, err := canton.NewGate(ctx, canton.Config{
	Routes: map[string]canton.Route{
		"GET /v1/data": {PayTo: "<your party id>", Price: canton.CC("0.01")},
	},
})
if err != nil {
	log.Fatal(err)
}
// on shutdown, after the server stopped: paid.Close(shutdownCtx)
```

net/http:

```go
mux := http.NewServeMux()
mux.HandleFunc("GET /v1/data", func(w http.ResponseWriter, r *http.Request) {
	payer, _ := canton.PayerFrom(r.Context())
	fmt.Fprintf(w, `{"payer":%q}`, payer)
})
log.Fatal(http.ListenAndServe(":8080", paid.Middleware()(mux)))
```

gin and echo use these imports:

```go
import (
	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/labstack/echo/v4/middleware"
)
```

```go
r := gin.New()
r.Use(gin.Recovery(), cantongin.Middleware(paid.Middleware()))
r.GET("/v1/data", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

e := echo.New()
e.Use(middleware.Recover(), cantonecho.Middleware(paid.Middleware()))
e.GET("/v1/data", func(c echo.Context) error { return c.JSON(200, map[string]bool{"ok": true}) })
```

`curl -i localhost:8080/v1/data` now answers 402. For a first test payment use a second wallet: `npx @ftptech/canton-agent-wallet pay <url>`. Paying your own `payTo` is refused.

- Route keys are literal `"METHOD /path"` (GET, POST, PUT, PATCH, DELETE). Register the handler with the same method; a pattern or wildcard is refused at startup.
- Prices: `canton.CC("0.01")`, `canton.Token(admin, "USDCx", "1.5")`, `canton.Atomic("100000000", canton.InstrumentID{Admin: admin, ID: "USDCx"})`.
- `canton.PayerFrom(ctx)` is the payer in your handler. In gin and echo put `gin.Recovery` / `Recover` before the gate, and call `cantongin.Check(r, routes)` / `cantonecho.Check(e, routes)` once before serving.

## Production

The default in-memory store forgets deliveries when the process restarts, so a replay inside the payment's deadline can be delivered again. Use Redis in production, even with one process, and for several processes or servers:

```go
rdb := redis.NewClient(&redis.Options{Addr: "redis:6379", ContextTimeoutEnabled: true}) // github.com/redis/go-redis/v9
cfg := canton.Config{Routes: routes, Store: goredis.NewStore(rdb, "x402:myapp:")}     // .../go/goredis
```

- One Redis, a Cluster or a Sentinel failover client, in a dedicated database with `maxmemory-policy noeviction` and AOF `everysec`. Not a `redis.Ring`, and no replica reads. Other clients: `canton.NewRedisStore`.
- Proxy read timeout, `http.Server.Shutdown` grace and the platform stop timeout (`docker stop -t`, `terminationGracePeriodSeconds`): at least `canton.RequestBudget(cfg)` (505 s by default).
- Proxies must accept a 32 KiB `PAYMENT-SIGNATURE` header.
- Limit per payer (`canton.PayerFrom`) in the handler and let `canton.IsRetry(ctx)` through. An IP limiter in front of the gate must pass requests that carry `PAYMENT-SIGNATURE`.
- Mount the gate on the whole server, before routers and anything that rewrites the path. A request it never sees is served free.
- Call `paid.Close(ctx)` after `http.Server.Shutdown` returns, with a ctx of at least `RequestBudget`.

## Money safety

- Your handler answers 2xx: the gate settles, then the payer gets your answer. If the facilitator refuses, the payer gets 402 `settlement_failed` and is not charged; your handler already ran.
- Your handler answers 4xx or 5xx: not charged, the answer passes through. A 3xx becomes 502, not charged.
- A replay of a delivered payment gets 402 `payment_already_redeemed`.
- A duplicate sent while the first runs gets 503 `payment in progress`.
- Every gate 503 says to retry the same payment. Only `do NOT pay again; retry the same payment` means the payment may have moved: the client resends the same signature and is served once.
- That retry runs your handler again: keep paid side effects idempotent.

## Docs

- [OPERATIONS.md](OPERATIONS.md): stores, shutdown, timeouts, wrappers, gin and echo details.
- `examples/nethttp` (IP and per-payer limits), `examples/gin`, `examples/echo`: graceful shutdown and Redis from `REDIS_URL`.
- [Merchant integration](https://x402-docs.ftptech.xyz/x402-merchant) on the FTP docs site.
- Apache-2.0, see [LICENSE](LICENSE).
