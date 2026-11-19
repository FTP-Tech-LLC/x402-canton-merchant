"""In-process drivers for the cases a real framework refuses to produce (odd statuses, odd messages)."""

from __future__ import annotations

import asyncio
import base64
import json
from collections.abc import Callable, Iterable
from typing import Any

import httpx

from ftptech_x402_canton import (
    CantonPaymentMiddleware,
    CantonServer,
    CantonWSGIMiddleware,
    Config,
    Route,
    cc,
)
from tests.mock_facilitator import MockFacilitator
from tests.support import FAST, PAID_ROUTE, PAY_TO, payment_header


def build_server(
    facilitator: MockFacilitator,
    routes: dict[str, Route] | None = None,
    start: bool = True,
    **config: Any,
) -> CantonServer:
    server = CantonServer(
        Config(
            routes=routes or {PAID_ROUTE: Route(pay_to=PAY_TO, price=cc("0.01"))},
            facilitator_url=facilitator.url,
            **{**FAST, **config},
        )
    )
    if start:
        server.start()
    return server


def asgi_app(
    status: int, body: bytes = b"ok", headers: list[tuple[bytes, bytes]] | None = None
) -> Callable[..., Any]:
    async def app(scope: Any, receive: Any, send: Any) -> None:
        if scope["type"] != "http":
            return
        await send({"type": "http.response.start", "status": status, "headers": headers or []})
        await send({"type": "http.response.body", "body": body})

    return app


def wsgi_app(
    status: str, body: bytes = b"ok", headers: list[tuple[str, str]] | None = None
) -> Callable[..., Iterable[bytes]]:
    def app(environ: Any, start_response: Any) -> Iterable[bytes]:
        start_response(status, headers or [])
        return [body]

    return app


def offer_via(client: httpx.Client) -> dict[str, Any]:

    response = client.get("/paid")
    assert response.status_code == 402
    accepted: dict[str, Any] = json.loads(base64.b64decode(response.headers["payment-required"]))[
        "accepts"
    ][0]
    return accepted


class WriteHonoringTransport(httpx.BaseTransport):
    """A WSGI transport that keeps what the app sends through ``start_response``'s ``write()``
    (httpx's own drops it, and every gate answer goes out that way)."""

    def __init__(self, app: Callable[..., Iterable[bytes]]) -> None:
        self.app = app

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        import io

        environ: dict[str, Any] = {
            "REQUEST_METHOD": request.method,
            "SCRIPT_NAME": "",
            "PATH_INFO": request.url.path,
            "QUERY_STRING": request.url.query.decode(),
            "SERVER_NAME": request.url.host,
            "SERVER_PORT": str(request.url.port or 80),
            "SERVER_PROTOCOL": "HTTP/1.1",
            "wsgi.version": (1, 0),
            "wsgi.url_scheme": request.url.scheme,
            "wsgi.input": io.BytesIO(request.read()),
            "wsgi.errors": io.StringIO(),
            "wsgi.multithread": True,
            "wsgi.multiprocess": False,
            "wsgi.run_once": False,
            "HTTP_HOST": request.url.netloc.decode(),
        }
        for name, value in request.headers.items():
            key = name.upper().replace("-", "_")
            environ[key if key in ("CONTENT_TYPE", "CONTENT_LENGTH") else f"HTTP_{key}"] = value
        status: list[str] = []
        headers: list[tuple[str, str]] = []
        written: list[bytes] = []

        def start_response(
            s: str, h: list[tuple[str, str]], exc_info: Any = None
        ) -> Callable[[bytes], None]:
            status.append(s)
            headers.extend(h)
            return written.append

        result = self.app(environ, start_response)
        try:
            body = b"".join(written) + b"".join(result)
        finally:
            close = getattr(result, "close", None)
            if close is not None:
                close()
        return httpx.Response(int(status[0].split()[0]), headers=headers, content=body)


def wsgi_client(server: CantonServer, app: Callable[..., Iterable[bytes]]) -> httpx.Client:
    return httpx.Client(
        transport=WriteHonoringTransport(CantonWSGIMiddleware(app, server)), base_url="http://test"
    )


async def asgi_client(server: CantonServer, app: Callable[..., Any]) -> httpx.AsyncClient:
    return httpx.AsyncClient(
        transport=httpx.ASGITransport(app=CantonPaymentMiddleware(app, server)),
        base_url="http://test",
    )


def header_for(accepted: dict[str, Any], n: int = 1) -> dict[str, str]:
    return {"PAYMENT-SIGNATURE": payment_header(accepted, f"{n:064x}")}


def idle_receive() -> Callable[[], Any]:
    """A server's ``receive``: the (empty) request body first, then silence until the client leaves."""
    delivered = False

    async def receive() -> dict[str, Any]:
        nonlocal delivered
        if not delivered:
            delivered = True
            return {"type": "http.request", "body": b"", "more_body": False}
        await asyncio.sleep(3600)
        return {"type": "http.disconnect"}

    return receive
