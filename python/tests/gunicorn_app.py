"""A Flask app behind the plugin, for the gunicorn test. Config comes from the environment."""

from __future__ import annotations

import json
import os
import time

from flask import Flask, Response, request

from ftptech_x402_canton import (
    CantonWSGIMiddleware,
    Config,
    MemoryStore,
    Route,
    canton_server_sync,
    cc,
    payer_from,
)

app = Flask(__name__)
app.config["MAX_CONTENT_LENGTH"] = (
    4_000_000  # a common upload limit: werkzeug then wraps wsgi.input
)
MASTER_PID = os.getpid()  # the process that imported the app: the master under --preload
UNLOCK_DELAY = float(os.environ.get("UNLOCK_DELAY", "0"))


class SlowUnlockStore(MemoryStore):
    """A store whose unlock takes UNLOCK_DELAY seconds (a remote store's round trip)."""

    def unlock(self, hash: str, token: str) -> None:
        if UNLOCK_DELAY:
            time.sleep(UNLOCK_DELAY)
        super().unlock(hash, token)


SERVER = canton_server_sync(
    Config(
        routes={
            "GET /paid": Route(pay_to="merchant::1220" + "bb" * 32, price=cc("0.01")),
            "POST /post": Route(pay_to="merchant::1220" + "bb" * 32, price=cc("0.01")),
            "POST /ignore": Route(pay_to="merchant::1220" + "bb" * 32, price=cc("0.01")),
        },
        facilitator_url=os.environ["FACILITATOR_URL"],
        settle_backoff=(0.01, 0.01, 0.01),
        retry_after_cap=0.02,
        store=SlowUnlockStore(),
        body_drain_timeout=float(os.environ.get("DRAIN_TIMEOUT", "30")),
    )
)


@app.route("/paid")
def paid() -> Response:
    return Response(f'{{"payer":"{payer_from()}"}}', mimetype="application/json")


@app.route("/post", methods=["POST"])
def post() -> Response:
    """A paid POST that reads its JSON body."""
    body = request.get_json()
    return Response(json.dumps({"got": body}), mimetype="application/json")


@app.route("/ignore", methods=["POST"])
def ignore() -> Response:
    """A paid POST that never reads its body."""
    return Response("{}", mimetype="application/json")


FREE_RUNS = [0]


@app.route("/free")
def free() -> Response:
    """A free route that counts its runs: it must never run from the tail of a request body."""
    FREE_RUNS[0] += 1
    return Response("free", mimetype="text/plain")


@app.route("/_free_runs")
def free_runs() -> Response:
    return Response(str(FREE_RUNS[0]), mimetype="text/plain")


@app.route("/_info")
def info() -> Response:
    """The worker's pid and the pid that created its facilitator client (never the master's)."""
    SERVER.transport.client  # noqa: B018  # created on first use in this process
    return Response(
        json.dumps(
            {"pid": os.getpid(), "client_pid": SERVER.transport.owner_pid, "master": MASTER_PID}
        ),
        mimetype="application/json",
    )


app.wsgi_app = CantonWSGIMiddleware(app.wsgi_app, SERVER)  # type: ignore[method-assign]
