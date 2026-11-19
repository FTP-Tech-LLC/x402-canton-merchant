# Operations

Every rule, limit and caveat of the plugin, by topic. The [README](README.md) has the short version.

## Routes and config

- Routes are literal `"METHOD /path"` keys (GET, POST, PUT, PATCH, DELETE).
- No `*`, `:x`, `[x]`, `?`, `#`, `%`, trailing slash or dot segments in a key.
- A bad config raises `CantonConfigError` listing every problem. Startup raises `CantonStartupError`. Nothing exits the process.
- Register the paid handler with the same method as the key. The gate charges that method and path only: a handler that also answers POST on the same path serves it free.
- Amounts on the wire are integers where 1 unit is 1e-10, for every instrument.
- `/supported` is read at startup and not refreshed. Restart after a facilitator fee payer change.
- Services that share a `pay_to` and terms accept each other's payments. Give them one store and one prefix, and a payment is delivered once across them. With separate stores each would serve it once.
- The Redis prefix is required so the keyspace is chosen on purpose.
- Browsers get the JSON 402 or a tiny page with the right amount, never the SDK's EVM/USDC paywall.

## Mounts and prefixes

- A key is the full path the gate sees. Include a router prefix: `APIRouter(prefix="/v2")` with `@router.get("/data")` is `"GET /v2/data"`.
- With the gate on the parent app, include the mount prefix too: `app.mount("/api", sub)` gives `"GET /api/v1/data"`, or the route is served free.
- When the gate wraps the mounted sub-app itself, either form works.
- A key is either the path inside the app the gate wraps or the full path from the server root, never a partial prefix. With nested mounts (`/svc` then `/api`), a key with only the inner prefix is served free.
- Start the server in the top-level app's lifespan or at import. A server started only in a mounted app's lifespan never starts, and a paid route then answers 503 (logged once).
- An app with its own lifespan calls `await server.astart()` and `await server.aclose()` itself instead of `server.lifespan`.
- Mangum: use `lifespan="off"` and start the server at import: `server = canton_server_sync(config)`.
- Flask: `canton_server_sync` starts the server. Close it at exit with `atexit.register(server.close)`.
- A server can start again after `close()`: a second lifespan (a test client per test) works.

## Paths and methods

- `/paid/`, `//paid`, `/PAID`, `/%70aid` and `HEAD` without a payment are charged (402).
- `/./paid` and `/x/../paid` are answered 404 by the gate.
- A `HEAD` with a payment is 405 and spends nothing.

## Settle

- Settle runs after the handler, never before. Upfront is never offered.
- Only a 2xx is settled. A 4xx or 5xx from your handler passes through unchanged and nobody is charged. Answer 429 for an over-limit payer.
- A 3xx from a paid handler becomes 502 and is not settled. `settle_on_redirect=True` changes that.
- A 204 or 304 never carries a body.
- /settle is never tied to the payer's connection or to a cancelled request. It runs to its own timeout (`settle_timeout`, 65 s per call).
- Settle is retried with the identical bytes on `settlement_pending`, transport errors, timeouts, 429 and 5xx, at most `settle_calls` times (default 4, the SDK's own retry included).
- Any answer without a parsed verdict counts as unknown.
- /verify outages (429, 5xx, timeout) are 503 `nothing was settled; retry the same payment`. A real `isValid:false` stays 402.

## Lost answers and unknown outcomes

- The attempt is recorded, with a digest of the signed bytes, before the first /settle.
- If the answer is lost, the retry of the same payment skips /verify (which would say "expired") and /settle answers from the facilitator's own record.
- After a process restart this needs `RedisStore`. The memory store forgets the attempt, and a retry after the payment deadline is refused as expired.
- If the outcome is still unknown, the answer is 503 `payment outcome unknown: do NOT pay again; retry the same payment`, never 402.
- The canonical hash is logged at warning level so you can reconcile.
- `payer_from(request)` is the payer /verify proved, or the one recorded for a retry.
- On a retry of a payment that may already have moved (`is_retry(request)`), a 4xx, 5xx, redirect, invalid status or exception from your handler is answered 503 `do NOT pay again` with the updateId. It is not passed through: a limiter should not refuse it.
- The retry of a withheld or unknown payment runs your handler again, and so may a resend after a payer left before settle. Make paid side effects idempotent per payment.

## Delivery and claims

- Nothing is delivered without a claim on the updateId.
- If the payer left or the store fails, the body is withheld, the answer is 503, and the retry is served once.
- If writing the response fails, the claim is released.
- A claim is marked delivered only after the response was written. Only a marked claim gets the final 402 `payment_already_redeemed`.
- A claim of another request that is not marked (mid-delivery, gave up, or died) answers 503 `do NOT pay again`.
- A process that dies between the claim and the mark leaves an unmarked claim. That payment answers 503 until the claim lapses (26 hours).
- A replay of a delivered payment is 402 `payment_already_redeemed` before /verify and your handler run.
- Concurrent duplicates get 503 while the first is in flight.
- Delivery is confirmed only up to the server's buffers.
- On ASGI the gate sends a paid body in one message. A payer who leaves in the middle of a body larger than the server and kernel buffers (often around 100 KB) is charged, may be marked delivered, and has its retry answered 402.
- Paid responses are buffered whole before settle: no streaming, keep them small.
- On ASGI the gate settles as soon as the last body chunk is in, while the app finishes. A FastAPI `BackgroundTask` runs alongside the settle, may run even when the settle then fails, and must not do work the payer is charged for.
- Gate errors never carry headers your handler set. Paid responses are `Cache-Control: private`, merged with your value. Gate errors are `no-store`.
- The payment header, the signature and the prepared transaction are never logged.

## Payer who leaves

- A payer who leaves during settle is seen before settle and before the claim. The body is withheld and the retry is served once.
- Where a server gives no signal, a payer who leaves during settle is charged and the retry is answered 402 `payment_already_redeemed`: only a failed write can release the claim.

| Server | Sees a payer who left | Notes |
|---|---|---|
| uvicorn, hypercorn, with an ASGI app | yes (`http.disconnect`) | tested, both uvicorn parsers |
| gunicorn (sync, threaded), werkzeug | yes (socket poll: a FIN behind unread bytes too) | tested |
| uWSGI | yes (`is_connected` plus a peek on a duplicate descriptor) | tested with pyuwsgi |
| waitress | only with `channel_request_lookahead=1` | the default 0 never sees it: logged as an error on the first paid request |
| TLS ended in the WSGI server | no | end TLS in a proxy in front. Logged once |
| Flask or any WSGI app under uvicorn (`--interface wsgi`), hypercorn's WSGI mode, Starlette `WSGIMiddleware`, asgiref `WsgiToAsgi` | no | they give no `write()`: answers go out as the return value and a failed write is not seen |
| AWS Lambda (Mangum) behind API Gateway | no | unsupported: no disconnect signal, and API Gateway cuts a request at 29 to 30 s while a settle can take about 57 s. Logged once |

## Request bodies

- A WSGI request body the handler leaves unread is drained, waiting for a body still arriving. It reads up to `max_unread_body_bytes` within `body_drain_timeout`, the same as ASGI.
- Past that, or when the client closes mid-body, the payer is treated as gone and nothing is settled.
- On ASGI the unread body is discarded after the handler returns (up to `max_unread_body_bytes`, 16 MiB, within `body_drain_timeout`). Past that the payer is treated as gone and nothing is settled.
- A handler that refuses at once (413, 401, 429) is answered before the upload ends.
- When a WSGI drain gives up, the connection is closed after the 503 and the bytes already queued on it are dropped, so the rest of the body is not run as a request.
- On uWSGI a chunked body is drained through `uwsgi.chunked_read`.
- On uWSGI, when the unread rest fits `buffer-size`, the drain is also bounded by uWSGI's own `--socket-timeout` (4 s by default).

## Several workers or servers

- The default store is in memory, for one process. It forgets every delivery on restart: a replay inside the payment deadline would then be delivered again. The plugin warns about it.
- Run exactly one worker with it: `--workers 1`, `WEB_CONCURRENCY` unset or 1, no `--max-requests` or `--limit-max-requests` recycling.
- The gate logs an error when it sees `WEB_CONCURRENCY` above 1 or a forked worker.
- With several workers or instances use Redis: a dedicated database with `maxmemory-policy noeviction`, read from the primary, and a non-empty prefix. A replica or an evicting policy can hide a delivered claim.
- `RedisStore.from_url(url, prefix=..., **client_options)` builds the redis-py client with client retries off, a 1 s socket and connect timeout, decoded responses and a pool of 256 connections. `client_options` override these.
- redis-py 6 and newer re-send a command after a lost reply by default: a `SET NX` then answers "taken" for a lock you hold. One store call on a new connection takes about 8 round trips (connect, handshake, the command), each up to its timeout.
- For a remote or TLS Redis raise `socket_timeout` and `socket_connect_timeout` in `from_url`. The connect timeout covers the whole TLS handshake: 1 s fails on a slow link. `request_budget` and the lock TTL follow the client's timeouts.
- Without redis-py, `from_url` raises `CantonConfigError` naming the `[redis]` extra.
- A single Redis only: no Redis Cluster endpoint. The store's keys would hash to different slots, and some payments would be settled and never delivered.
- `CantonServer.start` checks the store with real I/O, retried within `supported_wait` like /supported (a Redis that is not listening yet does not fail the start). The error then reads "payment store unusable after N s".
- The check needs no PING and no INFO, and uses only keys under your prefix, so ACL users without `@dangerous`, `info` or `ping` work. `INFO` is only a hint: `cluster_enabled:1` refuses, any failure or odd reply is ignored.
- The check reads 16 keys spread over the 16384 hash slots in one pipeline. Any MOVED or ASK refuses the endpoint as a Redis Cluster. Then it sets, reads and compare-and-deletes one probe key. A read-only replica or a missing Lua fails at startup, not at the first paid request.
- `RedisPyClient` decodes bytes, so a client created without `decode_responses=True` works too.
- `from_url` sets `max_connections` to 256 (redis-py 8 caps a pool at 100 and fails a call at once beyond it). Keep it above `worker_threads` plus a margin.
- If you turn client retries back on, the reconciler still recognizes its own payment lock, like the request path.
- For a custom client, wrap a redis-py client (a `socket_timeout`, `retry=Retry(NoBackoff(), 0)`) in `RedisPyClient` and pass it to `RedisStore(client, prefix)`. Any object with `set_nx`, `set`, `get`, `delete` and `delete_if_value` (a Lua compare-and-delete) also works; an optional `check(prefix)` method runs at startup.
- Never create the server before a fork. With `gunicorn --preload` or uWSGI without `lazy-apps` the master imports your app.
- The plugin opens its facilitator client and starts its reconciler lazily in each process.
- A store connection you pass in (a Redis client) and an `http_client` you pass in are yours: create them after the fork (gunicorn `post_fork`), or do not use `--preload`.

## Reconciler and shutdown

- A background reconciler (one daemon thread per process, started with the first paid request) releases claims and locks a store outage left behind, and writes records kept only in memory to the store. It retries until each succeeds or its TTL ends.
- Call `server.close()` (or `await server.aclose()`) after the web server has stopped. It waits for paid requests in flight, runs one last pass and logs at error what is left.
- It warns if requests were still running. At interpreter exit it logs at error what a server you never closed left behind.
- Once `close()` or `aclose()` begins, a request that carries a payment gets 503 `payment in progress`. The unpaid offer is still answered.
- They wait up to their timeout (default `request_budget(config)`) for paid requests in flight. uvicorn exits right after the lifespan, so that timeout is the last bound on settles in flight.
- Set gunicorn `--graceful-timeout 470`, uvicorn `--timeout-graceful-shutdown` and hypercorn `--graceful-timeout` to the request budget and your platform's stop timeout. Otherwise a deploy kills settles in flight.
- The final reconciler pass runs on a daemon thread, with at least 1 s past the timeout. `close` and `aclose` return then, and the process exit does not wait for it.
- It first writes records that only process memory holds, then releases locks and claims, and logs at error what it did not finish.
- A claim or lock it did not release ends with its TTL. A record it did not write is lost, and a later retry of that payment is refused as expired.

## Timeouts and sizes

- One paid request holds a worker for `request_budget(config)`: 465 s with the defaults (120 s payment lifetime, 4 settle calls of 65 s, the pauses, 7 store calls of 10 s). A single settle usually takes under 60 s.
- The budget assumes a store call under 10 s. `from_url`'s 1 s timeouts keep a call (about 8 round trips) under that. With raised timeouts the budget grows by 7 calls times the difference, for a `RedisStore` over redis-py; a custom store can set `call_budget_seconds`.
- Use it as the minimum for your server's worker timeout, your proxy's read timeout and the shutdown grace.
- Proxies and servers must accept a `PAYMENT-SIGNATURE` of 32 KiB (it is about 11 KiB).
- gunicorn: `--limit-request-field_size 32768` (the default 8190 rejects it) and `--timeout 470`.
- nginx: `large_client_header_buffers 4 32k;` and `proxy_read_timeout 470s;`.
- uvicorn with `--http h11` (or without httptools) and other servers also have header limits: allow 32 KiB.
- FastAPI: the blocking facilitator and store calls run on `worker_threads` threads (default 64). That also caps the paid requests in flight at once.
- More get a fast 503 `busy: if you already paid, do NOT pay again`. Size `worker_threads` to the settles you run at once. The handler runs on the event loop as usual.
- The facilitator limits /verify and /settle per calling IP and per payer. Limit per payer with `payer_from`, not only per IP.

## Dependencies and tests

- The plugin uses SDK internals and is tested against `x402` 2.28 only, so it pins `x402>=2.28,<2.29`.
- It installs into an existing FastAPI, Starlette or Flask app with no extra: the drivers are plain ASGI and WSGI. The `fastapi` and `flask` extras only name the minimum versions the tests ran on. The Redis store tests ran on redis-py 5.0.8 and 8.1.
- `pytest` needs the dev group: `uvicorn[standard]`, `fastapi`, `flask`, `gunicorn`, `waitress`, `hypercorn`, `redis`, `fakeredis[lua]`.
- `test_offer_live` reads the live `GET /supported` and skips offline (`CANTON_MERCHANT_OFFLINE=1` forces the skip). No test POSTs to a live facilitator.
- With `REDIS_URL` set, extra tests run the store against that Redis (use a database you can write to; they use a random key prefix).
