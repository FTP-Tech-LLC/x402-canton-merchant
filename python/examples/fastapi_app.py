"""A paid FastAPI route. Run: ``uvicorn examples.fastapi_app:app``, with ``CANTON_PAY_TO`` set."""

from __future__ import annotations

import os
import time
from collections import defaultdict

from fastapi import FastAPI, Request, Response

from ftptech_x402_canton import (
    CantonPaymentMiddleware,
    CantonServer,
    Config,
    RedisStore,
    Route,
    cc,
    is_retry,
    payer_from,
)

# The merchant party needs a Canton Coin preapproval (renew it every 30 days):
#   npx @ftptech/canton-agent-wallet preapproval
PAY_TO = os.environ["CANTON_PAY_TO"]  # "merchant::1220<64 hex>"
PAID_CALLS_PER_MINUTE = 6  # keep it under the facilitator's per-payer /settle cap (10 a minute)

# One worker can use the default memory store, which forgets payments on every restart. Production:
# one Redis (a dedicated database, maxmemory-policy noeviction), required for several workers or
# servers: set REDIS_URL.
REDIS_URL = os.environ.get("REDIS_URL")
store = RedisStore.from_url(REDIS_URL, prefix="x402:my-service:") if REDIS_URL else None

server = CantonServer(
    Config(
        store=store,
        routes={
            "GET /v1/data": Route(pay_to=PAY_TO, price=cc("0.01"), description="One data point")
        },
        facilitator_url=os.environ.get("CANTON_FACILITATOR_URL", "https://facilitator.ftptech.xyz"),
    )
)


# server.lifespan reads /supported at startup (retried; raises CantonStartupError, never exits the
# process) and waits for paid requests in flight at shutdown.
app = FastAPI(lifespan=server.lifespan)
app.add_middleware(CantonPaymentMiddleware, server=server)

_recent: dict[str, list[float]] = defaultdict(list)


@app.get("/v1/data")
def data(request: Request) -> Response:
    # A sync route runs in a thread; the payer is the one /verify proved.
    payer = payer_from(request)
    if payer is not None and not is_retry(request):
        now = time.monotonic()
        window = [t for t in _recent[payer] if now - t < 60]
        if len(window) >= PAID_CALLS_PER_MINUTE:
            # A 4xx or 5xx from the handler is not settled: nobody is charged.
            return Response('{"error":"slow down"}', status_code=429, media_type="application/json")
        _recent[payer] = [*window, now]
    return Response('{"value":42}', media_type="application/json")
