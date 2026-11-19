# ftptech-x402-canton

Take x402 payments in Canton Coin or a CIP-56 token on a FastAPI or Flask server. The plugin sits in front of the official [x402 Python SDK](https://github.com/x402-foundation/x402) and settles through FTP's facilitator.

It charges only when your handler answers 2xx, and it delivers each payment once.

## Install

```bash
pip install "ftptech-x402-canton[fastapi,redis]"
```

Extras: `fastapi`, `flask`, `redis`. Python 3.10 or newer, `x402` 2.28.

## Before you start

The party that receives payments needs a live Canton Coin `TransferPreapproval`:

```bash
npx @ftptech/canton-agent-wallet create --relay-url https://facilitator.ftptech.xyz
npx @ftptech/canton-agent-wallet address       # your party id: pay_to
# send it a few CC, then
npx @ftptech/canton-agent-wallet claim
npx @ftptech/canton-agent-wallet preapproval   # lasts 30 days, renew it
```

A token needs `preapproval --admin <admin> --id <id>` for each instrument you price.

## Quickstart

FastAPI:

```python
from fastapi import FastAPI, Request
from ftptech_x402_canton import CantonPaymentMiddleware, CantonServer, Config, Route, cc, payer_from

routes = {"GET /v1/data": Route(pay_to="<your party id>", price=cc("0.01"))}
server = CantonServer(Config(routes=routes))
app = FastAPI(lifespan=server.lifespan)
app.add_middleware(CantonPaymentMiddleware, server=server)


@app.get("/v1/data")
def data(request: Request) -> dict:
    return {"payer": payer_from(request)}
```

Flask:

```python
import atexit
from flask import Flask
from ftptech_x402_canton import (
    CantonWSGIMiddleware,
    Config,
    Route,
    canton_server_sync,
    cc,
    payer_from,
)

routes = {"GET /v1/data": Route(pay_to="<your party id>", price=cc("0.01"))}
server = canton_server_sync(Config(routes=routes))
atexit.register(server.close)  # waits for paid requests in flight
app = Flask(__name__)
app.wsgi_app = CantonWSGIMiddleware(app.wsgi_app, server)


@app.get("/v1/data")
def data():
    return {"payer": payer_from()}
```

- Routes are literal `"METHOD /path"` keys with the full path, router or mount prefix included. Register the handler with the same method. Any other path or method is served free.
- Prices: `cc("0.01")`, `token(admin, "USDCx", "1.5")` or `atomic("100000000", InstrumentId(admin, id))`.
- `payer_from(request)` is the payer the facilitator verified. Use it to limit calls per payer, but never refuse a retry: skip the limit when `is_retry(request)` is true, and answer an over-limit payer 429.

Run it (`uvicorn main:app` for FastAPI, `gunicorn main:app` for Flask), then `curl -i http://localhost:8000/v1/data` answers 402.

## Production

One worker can run on the default memory store, but it forgets payments on every restart: a replay can be served twice and a paid retry is refused as expired. Redis also covers restarts, so use it in production. With several workers or servers it is required. Use a dedicated Redis database (a single Redis, not a Cluster endpoint) with `maxmemory-policy noeviction`:

```python
from ftptech_x402_canton import Config, RedisStore

store = RedisStore.from_url("redis://cache:6379/2", prefix="x402:my-service:")
config = Config(routes=routes, store=store)
```

Services that share a `pay_to` and terms must share one prefix: a payment is then delivered once across them. With different prefixes each service would serve it once.

The plugin must see a payer who leaves during settle, or that payer is charged and gets nothing on the retry:

| Server | Sees a payer who left | Needs |
|---|---|---|
| uvicorn, hypercorn, with an ASGI app (FastAPI, Starlette) | yes | nothing |
| gunicorn, werkzeug, uWSGI, with Flask | yes | nothing |
| waitress, with Flask | only with a setting | `channel_request_lookahead=1` |
| Flask under uvicorn or hypercorn (WSGI mode or an adapter), TLS ended in the WSGI server, Mangum | no | serve Flask with gunicorn, uWSGI or waitress; end TLS in a proxy; Mangum is unsupported |

- Allow a 32 KiB `PAYMENT-SIGNATURE` header in every proxy and server (it is about 11 KiB).
- Set worker, proxy and shutdown timeouts to at least `request_budget(config)`, 465 s with the defaults.
- Stop the server gracefully: `server.lifespan` or `close()` waits for paid requests in flight.
- The facilitator limits calls per payer: limit per payer in your handler too.

## Money safety

- 2xx from your handler: charged, the response is delivered.
- 4xx or 5xx from your handler: not charged, passed through. On a retry of a payment that may have moved it becomes 503 (see OPERATIONS.md).
- 3xx from your handler: becomes 502, not charged.
- The same payment again after delivery: 402.
- The same payment while the first is in flight: 503.
- 503 with "do NOT pay again": retry the same signature. The retry runs your handler again, so keep paid side effects idempotent.
- The preapproval lasts 30 days and payments to an expired one fail: run `preapproval` again before then (`--force` renews early).
- `~/.canton-agent/wallet.json` holds the key of the party you are paid to. Back it up with `npx @ftptech/canton-agent-wallet export`.
- Keep paid bodies small: they are buffered, and delivery is confirmed only up to the server's buffers.

## Docs

- [OPERATIONS.md](OPERATIONS.md): every rule, limit and caveat, by topic.
- `examples/fastapi_app.py`, `examples/flask_app.py`: with a per-payer limiter and `REDIS_URL`.
- [Merchant integration](https://x402-docs.ftptech.xyz/x402-merchant) on the FTP docs site.
- License: Apache-2.0.
