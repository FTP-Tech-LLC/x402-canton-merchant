"""Canton x402 merchant plugin for the official x402 Python SDK (FastAPI, Flask).

Quickstart (FastAPI)::

    from fastapi import FastAPI
    from ftptech_x402_canton import CantonPaymentMiddleware, CantonServer, Config, Route, cc

    server = CantonServer(Config(routes={
        "GET /v1/data": Route(pay_to="merchant::1220<64 hex>", price=cc("0.01")),
    }))

    app = FastAPI(lifespan=server.lifespan)
    app.add_middleware(CantonPaymentMiddleware, server=server)

Flask: ``server = canton_server_sync(config)`` and
``app.wsgi_app = CantonWSGIMiddleware(app.wsgi_app, server)``.

See the README, and OPERATIONS.md for the rules and limits.
"""

from __future__ import annotations

from .asgi import CantonPaymentMiddleware
from .config import (
    DEFAULT_FACILITATOR_URL,
    Config,
    Route,
    request_budget,
)
from .errors import CantonConfigError, CantonNotReadyError, CantonStartupError, StoreFullError
from .mechanisms.canton import InstrumentId
from .payer import is_retry, payer_from
from .price import (
    CANTON_COIN_INSTRUMENT_ID,
    DSO_MAINNET_PARTY,
    Price,
    atomic,
    cc,
    token,
)
from .server import CantonServer, canton_server, canton_server_sync
from .store import Record, Store
from .store_memory import MemoryStore
from .store_redis import RedisClient, RedisPyClient, RedisStore
from .wsgi import CantonWSGIMiddleware

__all__ = [
    "CANTON_COIN_INSTRUMENT_ID",
    "DEFAULT_FACILITATOR_URL",
    "DSO_MAINNET_PARTY",
    "InstrumentId",
    "CantonConfigError",
    "CantonNotReadyError",
    "CantonPaymentMiddleware",
    "CantonServer",
    "CantonStartupError",
    "CantonWSGIMiddleware",
    "Config",
    "MemoryStore",
    "Price",
    "Record",
    "RedisClient",
    "RedisPyClient",
    "RedisStore",
    "Route",
    "Store",
    "StoreFullError",
    "atomic",
    "canton_server",
    "canton_server_sync",
    "cc",
    "is_retry",
    "payer_from",
    "request_budget",
    "token",
]
