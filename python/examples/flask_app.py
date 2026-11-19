"""A paid Flask route.

Run: ``gunicorn --timeout 470 --graceful-timeout 470 --limit-request-field_size 32768 examples.flask_app:app``.
Without ``--preload``: the server is built in each worker.
"""

from __future__ import annotations

import atexit
import os
import threading
import time
from collections import defaultdict

from flask import Flask, Response, jsonify

from ftptech_x402_canton import (
    CantonWSGIMiddleware,
    Config,
    RedisStore,
    Route,
    canton_server_sync,
    cc,
    is_retry,
    payer_from,
    request_budget,
)

# The merchant party needs a Canton Coin preapproval (renew it every 30 days):
#   npx @ftptech/canton-agent-wallet preapproval
PAY_TO = os.environ["CANTON_PAY_TO"]  # "merchant::1220<64 hex>"
PAID_CALLS_PER_MINUTE = 6  # keep it under the facilitator's per-payer /settle cap (10 a minute)

# One worker can use the default memory store, which forgets payments on every restart. Production:
# one Redis (a dedicated database, maxmemory-policy noeviction), required for several workers or
# servers: set REDIS_URL.
REDIS_URL = os.environ.get("REDIS_URL")

config = Config(
    store=RedisStore.from_url(REDIS_URL, prefix="x402:my-service:") if REDIS_URL else None,
    routes={"GET /v1/data": Route(pay_to=PAY_TO, price=cc("0.01"), description="One data point")},
    facilitator_url=os.environ.get("CANTON_FACILITATOR_URL", "https://facilitator.ftptech.xyz"),
)
server = canton_server_sync(
    config
)  # blocks while /supported is read (retried); raises, never exits
atexit.register(server.close)  # wait for paid requests in flight, last reconciler pass
print(f"worker timeout and proxy read timeout: at least {request_budget(config):.0f} s")

app = Flask(__name__)
app.wsgi_app = CantonWSGIMiddleware(app.wsgi_app, server)  # type: ignore[method-assign]

_recent: dict[str, list[float]] = defaultdict(list)
_recent_lock = threading.Lock()


@app.get("/v1/data")
def data() -> Response | tuple[Response, int]:
    payer = payer_from()  # the one /verify proved (or recorded, on a retry)
    if payer is not None and not is_retry():
        now = time.monotonic()
        with _recent_lock:
            window = [t for t in _recent[payer] if now - t < 60]
            if len(window) >= PAID_CALLS_PER_MINUTE:
                # A 4xx or 5xx from the handler is not settled: nobody is charged.
                return jsonify(error="slow down"), 429
            _recent[payer] = [*window, now]
    return jsonify(value=42)
