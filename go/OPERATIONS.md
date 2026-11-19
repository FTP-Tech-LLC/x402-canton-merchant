# Operations notes

The details behind the [README](README.md): what the payer sees, how stores and shutdown work, and the traps in gin and echo.

## Charging and responses

- The gate settles only after your handler answered 2xx. If the facilitator then refuses /settle, the payer gets 402 `settlement_failed` and is not charged. Your handler has already run.
- Every gate 503 tells the payer to retry the same payment, with `Retry-After`. Only the ones that say `do NOT pay again` mean money may have moved.
- While the facilitator cannot verify, the payer gets 503 `verification unavailable: nothing was settled`. With a full memory store the payer gets 503 `nothing was charged` before any settle.
- A 4xx or 5xx from your handler is not charged and passes through. Answer 429 for an over-limit payer.
- A 3xx becomes 502 and is not charged. `SettleOnRedirect` changes that.
- A payer who disconnects before the settle is not charged and may resend the payment until its deadline. A handler can therefore run for one payment more than once.
- `HEAD` with a payment is answered 405, because a HEAD cannot receive the response. Send the payment with GET.
- A browser gets a small page with the Canton amount. Everyone else gets the JSON 402.
- Paid responses are held in memory about three times their size until written. Keep them small: there is no streaming.
- A paid route whose request body can exceed 256 KiB must read the whole body in the handler.
- The gate drains only 256 KiB before it settles. Past that it cannot see a payer who left.
- `canton.PayerFrom` is the payer /verify proved, or the one recorded for a retry that skipped /verify.
- Your handler must fit in the payment's lifetime (`ExecuteBeforeSeconds`, 120 s by default). A slower handler gets its payment expired and nothing is charged.

## One payment, one delivery

- A replay gets 402 `payment_already_redeemed`.
- A duplicate sent while the first is running gets 503 `payment in progress`.
- 503 `... do NOT pay again; retry the same payment` means the payment may have moved. The client resends the same signature and is served once, even after the payment's deadline.
- That needs a store that survives restarts. The default memory store forgets deliveries and owed retries on restart, so a replay inside the deadline is delivered again.
- With Redis, a process restart between the facilitator's commit and the gate's record can still end in a refusal after the deadline.
- A Redis outage that outlasts the process does the same. A retry inside the deadline is served.
- The retry runs your handler again. Make paid side effects idempotent per payment.
- If your handler answers 4xx or 5xx on that retry, the gate answers 503 instead, so the charge is not hidden.
- `canton.IsRetry(ctx)` tells a limiter to let the retry through. A per-payer 429 on a retry is turned into that 503 and keeps being refused while the limiter refuses.
- A handler that refuses every retry of a recorded payment holds that payment at 503 for 26 hours. Answer `IsRetry` requests the way the first run did.
- The same hold happens if the Store cannot delete an attempt right after /settle refused it (nothing moved).
- The stale attempt stays until its 26 h TTL, and a retry of those bytes skips /verify.
- A handler refusal on that retry is answered 503. No background job clears the attempt.
- A claim is confirmed delivered only after the response was written. The claim is then marked, off the response path.
- A claim of another request that is not marked (mid-delivery, gave up, or died) answers 503 `do NOT pay again; retry the same payment`, never the final 402.
- A process that dies between the claim and the mark leaves an unmarked claim that holds the payment at 503 for 26 hours.
- An unknown outcome is recoverable for 23 hours (the facilitator keeps its own record for 24).
- After that a refusal for such an attempt keeps answering 503, never 402.

## Paths and mounting

- Route keys are literal `"METHOD /path"` (GET, POST, PUT, PATCH, DELETE). A pattern, a Go 1.22 mux wildcard (`{id}`, `{path...}`, `{$}`), a paid root `/`, a trailing slash or a dot segment is refused at startup. List each paid path.
- Every config problem comes back as an error from `NewGate` or `NewMiddleware`, never an exit. `goredis.NewStore(nil, ...)` is reported the same way.
- `goredis.NewStore` panics for a `*redis.Ring` or a cluster client that reads from replicas, because those can serve a payment twice.
- `NewGate` waits up to `SupportedWait` (60 s) for the facilitator and the Store, then returns an error that names the one that failed. A cancelled ctx ends the wait.
- Register the handler with the same method and path (`mux.HandleFunc("GET /v1/data", ...)`), never path-only: a path-only handler would serve POST or PUT for free.
- Mount the gate on the whole server, before routers and anything that rewrites the path. A request it never sees is served free.
- `/paid/`, `//paid` and `/%70aid` are charged. `/PAID` and `/./paid` are answered 404 by the gate.
- The gate also looks at a path decoded up to three more times (`%2570aid` for `%70aid`), because a framework may decode once more. On plain net/http a free path that decodes to a paid key gets 404, nothing charged: do not serve free files whose names are escapes of a paid path.

## Writers and wrappers

- A ResponseWriter wrapper in front of the gate (gzip, logging, a status recorder) must implement `Unwrap() http.ResponseWriter`.
- The gate reads the connection's flush error through it and gives the claim back when the answer did not arrive. It also lifts the write deadline through it.
- The gate flushes every wrapper, so a buffering one pushes its bytes, and reads the error at the writer that owns the connection.
- A wrapper without `Unwrap` hides the connection. A failed write after settle then counts as delivered and the retry is refused as redeemed. The gate logs a warning once when it cannot see the connection.
- Without `Unwrap` the write-deadline lift fails too. Put the gate outermost, add `Unwrap`, or set `WriteTimeout` to the budget.
- Compress at the proxy, not in a wrapper in front of the gate.
- No deadline middleware shorter than `canton.RequestBudget` may sit in front of the gate.
- Nothing in front of the gate may answer 4xx to a request that carries a payment.
- Over HTTP/2 straight to the app (no proxy), a write that fails after settle can be seen as delivered while the connection's read side still looks open. net/http gives a handler no way to wait for the socket flush.
- Put a proxy in front that speaks HTTP/1.1 to the app. "A failed write gives the claim back" holds for HTTP/1.1.
- Over HTTP/2 a connection loss the server has not processed before the final write, a full close included, can count as delivered.

## Stores and Redis

- The default store is in memory and forgets everything on restart. Use Redis for production, with one process too.
- It holds about 100,000 paid calls per 26 hours (about 70 MB). `WithMaxEntries(n)` changes that: n counts entries, two per paid call.
- Services with the same `payTo` and terms must share one store, or use different memos. Two separate stores would accept each other's payments.
- When the memory store is full it answers new payments 503 `nothing was charged` before it settles anything, and still serves the retries of payments it holds.
- `goredis.NewStore(client, prefix)` takes a go-redis v9 client: one Redis, a Cluster or a Sentinel failover client. Every operation touches one key.
- Not a `*redis.Ring`: it moves keys to another shard when one is marked down, and a delivered payment can be delivered again.
- No replica reads (`ReadOnly`, `RouteRandomly`, `RouteByLatency`): a replica may not have the claim yet.
- Set `ContextTimeoutEnabled: true` in `redis.Options`. Without it go-redis ignores the gate's per-call deadlines, and a stalled Redis holds a paid request past its budget.
- `REDIS_URL` cannot set it: set `options.ContextTimeoutEnabled = true` after `redis.ParseURL`.
- `canton.NewRedisStore(RedisClient, prefix)` takes any other client. `RedisClient` is five methods: SetNX, Set, Get, Del and DelIfValue (a compare-and-delete in Lua).
- An empty prefix becomes `x402-canton:`. Use a non-empty one.
- Use a dedicated Redis database with `maxmemory-policy noeviction`, and read from the primary.
- Size `maxmemory` for 26 hours of paid calls (two keys of a few hundred bytes per payment) with headroom. Alert on `used_memory` and OOM errors.
- A full Redis refuses writes, and admitted payments are then withheld until it has room.
- The startup probe reads a record, then takes and releases a throwaway lock. A read-only replica, or an ACL without SET NX or EVAL, fails at startup.
- A plain client on a Redis Cluster endpoint gets MOVED for part of the key space. Startup reads sixteen keys under the prefix, one per 1024 slots, and fails when any answers MOVED or ASK: use `redis.NewClusterClient` or one Redis.
- A Redis at `maxmemory` with `noeviction` fails the probe with OOM, so `NewGate` returns an error. A full memory store passes it and refuses payments later.
- A client library may re-send a command after a network error that lost the reply (go-redis does, up to 3 times). The store recognises its own lock and claim, so such a re-send does not make the owner wait for the TTL.
- Deleting twice, or compare-and-deleting after the key changed owner, is harmless.

### Persistence and failover

- One delivery holds only for writes that reached a replica or the disk.
- A single Redis instance needs persistence: AOF with `appendfsync everysec`. Without it a restart loses every claim and record, and a replay inside the deadline is delivered again.
- Redis replicates asynchronously. A master can acknowledge a claim and die before its replica has it, and a promoted replica then lacks the claim.
- Bound that window on replicated setups with `min-replicas-to-write` and `min-replicas-max-lag`.
- The window is small, but it is the one way a failover can deliver a payment twice.

## Lifetime and shutdown

- A process that dies mid-settle leaves a lock that lapses within `RequestBudget` (475 s of the 505 s default). A retry before then gets 503 `payment in progress`.
- While the process lives, a background reconciler releases claims and locks a Store outage left behind. It also writes records kept only in memory to the Store, so a Redis blip heals within seconds of Redis answering.
- The one exception is the stale attempt above: a failed delete of a refused attempt is not retried.
- `NewMiddleware` binds the reconciler to its `ctx`. `canton.NewGate` gives it its own lifetime, so a startup ctx (a signal ctx, one with a timeout) is fine.
- Call `Close(ctx)` after `http.Server.Shutdown` has returned. Pass a ctx of at least `RequestBudget`.
- A shorter ctx is enough only when `Shutdown` returned within the budget and nothing is left in flight.
- After `Close` starts, the old gate answers a request that carries a payment 503 `payment in progress: retry the same payment`. The unpaid 402 offer still works.
- `Close` waits for this gate's paid requests still in flight and their post-response work, with the reconciler running. It then stops the reconciler, runs one last pass (bounded to 30 s) and logs at error what is left.
- The work runs once. Each `Close` caller waits up to its own ctx.
- On a reload, swap the handler first, then `Close` the old gate.
- Each call starts one goroutine and keeps its ledger (and a default memory store) alive. Build the gate once per process, or `Close` the old one and give the new one the same `Config.Store`.
- A rebuilt gate with the default memory store forgets what the old one delivered, so a replayed payment inside its deadline would be delivered again.

## Timeouts and proxies

- `canton.RequestBudget(cfg)` is the longest one paid request can take: 505 s with defaults.
- Use it as the minimum for your proxy's read timeout and for the shutdown grace.
- The platform's stop timeout counts too: `docker stop -t`, Kubernetes `terminationGracePeriodSeconds`. Their defaults (10 s, 30 s) are far below it.
- One /settle can take about 57 s. A stop inside the budget can end a paid request mid-settle.
- The gate lifts `http.Server.WriteTimeout` itself once /verify passes.
- Proxies must accept a `PAYMENT-SIGNATURE` of 32 KiB: nginx `large_client_header_buffers 4 32k;`. The header is 8 to 16 KiB.
- `Timeout` is the SDK's deadline on /verify. It does not bound your handler.
- `SettleTimeout` (65 s) bounds each /settle call. A client or transport timeout on your `HTTPClient` does not cap it.
- The plugin lifts that timeout on a copy of a plain `*http.Transport`, including one set up with `x/net/http2`.
- A transport you wrap (OpenTelemetry, retries, logging) is not lifted. Its `ResponseHeaderTimeout` must be 0 or at least `SettleTimeout`.
- The facilitator limits /verify and /settle per calling IP and per payer. The hosted MCP calls from one IP, so limit per payer (`canton.PayerFrom`), not only per IP.

## gin and echo

- The adapters wrap the same gate and add no modules. The money rules above hold, with the framework's own answer as the answer.
- The gate sees the final status and the exact bytes before it settles.
- Paid routes keep their literal key `"METHOD /full/path"`. A group prefix is part of the path, and `:id`, `*` and `{id}` are refused.
- Through gin or echo a paid answer is held about four times its size.
- Pass `paid.Middleware()` itself to the adapter and compose nothing into it. net/http middleware (auth, limits, metrics) goes around the engine.
- The runtime guard runs that function on unpaid, header-less copies of free requests on param, catch-all and unrouted paths.
- Anything composed in also sees those copies. One that refuses them turns free requests into 404.
- The adapter reads "paid" from the gate's verified state, whether or not the facilitator named a payer.
- Registering the same gate twice is harmless: the inner one passes a request the outer one charges through.
- A second, different gate (a group with its own config, a mounted sub-app) still gates its own routes. If two gates share a key, only the outer one charges.

### Registration

- Call `Use` once on the engine, before creating groups and routes. gin copies the chain into a group when it is created, and echo binds group middleware when a route is added.
- Give each paid key its own route with the method of the key.
- In echo use `e.Use`, not `e.Pre`.
- Register every route before serving: echo's router is not safe for registration during traffic.
- `gin.Recovery`, echo `Recover`, `Logger` and any middleware that renders errors or recovers panics go before the gate. The first `Use` is the outermost.
- Inside the gate they answer below the adapter after a 2xx started, and the partial answer could be settled.
- No response-wrapping middleware (gzip, body dump, status recorders) inside the gate on a paid route. In front of it only with `Unwrap`.
- echo's Gzip and BodyDump drop the connection error even with `Unwrap`. Use their Skipper for paid routes and compress at the proxy.

### Runtime guard

- This is what protects, not `Check`.
- A request that reaches a param or catch-all route (`r.Any("/files/*filepath")`, `r.Match` on `/v1/:name`), NoRoute, echo's `RouteNotFound` or echo's own 404/405 (`middleware.Static`) on a paid path gets a 404 from the adapter.
- This holds in any spelling the gate charges or refuses, for a method that is not the key's. A HEAD follows its GET key.
- Nothing is charged and no handler runs.
- An exact route of your own for another method (an ingest on `POST /v1/data`) is not touched and must not return paid content.
- Answer CORS preflights in middleware outside the gate.
- NoRoute is protected only by a gate registered with `r.Use`, because gin builds the NoRoute chain from engine middleware.
- With NoRoute or `RouteNotFound` serving files, put every gate on the engine. A gate passes other gates' keys through, so several work.
- Middleware that serves content by path (`middleware.Static`, a proxy) and sits inside the gate also runs for an exact route of your own on a paid path, before its handler.
- The adapter cannot stop that middleware.
- Give such middleware `Skipper: cantonecho.SkipPaidPaths(paid.Middleware())`, or keep paid files out of its root. A path echo decodes once more (`EnablePathUnescaping`) is covered.

### Check

- `Check` is a startup sanity check, static and bounded in time (5 s a request).
- Per paid key it sends one unpaid request that must get 402. A correct setup never reaches a handler.
- It fails when the key's handler is also registered for another method (`Any`, `Match`, a second registration).
- It sends two GETs for each paid path (`//path` and `/dir//name`) and fails on a 2xx with content: no gate is in front of NoRoute or `RouteNotFound`.
- It runs no other handler.
- gin compares the last handler of a route by function value. A renderer shared by a paid key and another method's chain is reported.
- echo only has handler names, so two closures from one call site look alike there.
- One net/http handler wrapped twice (`gin.WrapH`, `echo.WrapHandler`) is not detected.

### gin

- A bare 3xx, 4xx or 5xx is not charged, however it is set (`c.Status`, `WriteHeader`, `AbortWithStatus`, `c.Redirect`, `ServeFile`'s 304). A bare 2xx is an empty paid answer and is charged.
- An error recorded with `c.Error`, an abort without an answer, a render error after a partial write and a panic after a started answer are answered 500 and not charged.
- Use `c.Set`, not `c.Error`, for a non-fatal note.
- `c.Bind*` answers 400 itself: return right after its error. The body of a bind-failure 400 is replaced, so paid content written after an ignored error is never sent.
- A request gin routes to no handler (a `//path` spelling, NoRoute, 405) is gin's 404/405 with no offer.
- A paid spelling never reaches your NoRoute handler, whatever the method, when the gate is on `r.Use`. A paid key needs its own gin route.
- Never call `engine.HandleContext` on a paid route.
- Bytes sent behind the gate cannot be taken back, and the adapter only refuses to settle them.
- Never write to the context from a goroutine after the handler returned. `c.Copy()` is for reading.
- `Hijack` is refused and `Flush` holds until settle.

### echo

- A returned error is rendered through your `HTTPErrorHandler` into a fresh response before the gate decides, even after a 2xx was written.
- If the handler renders nothing or a status below 400 (a redirect included), the answer is a 500 and not charged.
- Outer middleware sees `nil` for a paid handler's error. The status carries it.
- Do not write to the context from a goroutine after the handler returned.
- A request echo routes to another handler than the paid path (a catch-all, a `/paid/` spelling) is answered 404 and not charged.
- `Flush` holds until settle.
