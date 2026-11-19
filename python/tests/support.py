"""Test harness: the plugin in front of a real FastAPI/uvicorn or Flask/werkzeug server."""

from __future__ import annotations

import asyncio
import base64
import json
import logging
import socket
import threading
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any

import httpx
from fastapi import BackgroundTasks, FastAPI, Request, Response

from ftptech_x402_canton import (
    CantonPaymentMiddleware,
    CantonServer,
    CantonWSGIMiddleware,
    Config,
    Route,
    cc,
    is_retry,
    payer_from,
)
from tests.mock_facilitator import MockFacilitator

PAY_TO = "merchant::1220" + "bb" * 32
PAID_ROUTE = "GET /paid"
FAST: dict[str, Any] = {
    "settle_backoff": (0.01, 0.01, 0.01),
    "retry_after_cap": 0.02,
    "supported_retry_pause": 0.05,
    "supported_wait": 2.0,
    "settle_timeout": 5.0,
    "verify_timeout": 5.0,
}


def tx_hash(n: int = 1) -> str:
    return f"{n:064x}"


def payment_header(accepted: dict[str, Any], tx: str | None = None, **payload: Any) -> str:
    """A PAYMENT-SIGNATURE header for the offer in ``accepted`` (an entry of the 402's accepts)."""
    body = {
        "x402Version": 2,
        "accepted": accepted,
        "payload": {
            "preparedTxHash": tx if tx is not None else tx_hash(),
            "preparedTransaction": "AAAA",
            "signature": "SECRET-SIGNATURE-DO-NOT-LOG",
            **payload,
        },
    }
    return base64.b64encode(json.dumps(body).encode()).decode()


@dataclass
class Seen:
    """What the paid handler saw."""

    payer: str | None
    retry: bool
    headers: dict[str, str]
    body: bytes = b""


@dataclass
class Out:
    """What the paid handler answers."""

    status: int = 200
    body: bytes = b'{"data":42}'
    headers: dict[str, str] = field(default_factory=lambda: {"Content-Type": "application/json"})
    cookies: bool = False


class Harness:
    """One plugin instance in front of a real server on a free port."""

    def __init__(
        self,
        kind: str,
        facilitator: MockFacilitator,
        *,
        routes: dict[str, Route] | None = None,
        **config: Any,
    ) -> None:
        self.kind = kind
        self.facilitator = facilitator
        self.handler: Callable[[Seen], Out] = lambda seen: Out()
        self.read_body = False
        self.http_impl = str(config.pop("http", "auto"))
        # uvicorn or hypercorn for ASGI; werkzeug, waitress or wsgiref for WSGI.
        self.server_kind = str(config.pop("server", "default"))
        self.background: Callable[[], None] = lambda: None
        self.seen: list[Seen] = []
        self.log = logging.getLogger("ftptech_x402_canton")
        settings = {**FAST, **config}
        self.config = Config(
            routes=routes or {PAID_ROUTE: Route(pay_to=PAY_TO, price=cc("0.01"))},
            facilitator_url=facilitator.url,
            **settings,
        )
        self.server = CantonServer(self.config)
        self.server.start()
        self.port = 0
        self._stop: Callable[[], None] = lambda: None
        self._serve()
        self.client = httpx.Client(
            event_hooks={"response": [self._wait_until_marked]},
            base_url=f"http://127.0.0.1:{self.port}",
            timeout=30,
            limits=httpx.Limits(
                max_keepalive_connections=0
            ),  # a server may close the connection after an error
        )
        self._offers: dict[str, dict[str, Any]] = {}

    # -- the paid handler --------------------------------------------------------

    def _respond(self, seen: Seen) -> Out:
        self.seen.append(seen)
        return self.handler(seen)

    @property
    def runs(self) -> int:
        return len(self.seen)

    # -- servers ----------------------------------------------------------------

    def _serve(self) -> None:
        if self.kind == "asgi":
            self._serve_asgi()
        else:
            self._serve_wsgi()

    def _serve_asgi(self) -> None:
        app = FastAPI()

        @app.get("/paid")
        @app.post("/paid")
        @app.get("/PAID")
        def paid(request: Request) -> Response:
            out = self._respond(Seen(payer_from(request), is_retry(request), dict(request.headers)))
            response = Response(out.body, status_code=out.status, headers=out.headers)
            if out.cookies:
                response.set_cookie("session", "secret")
            return response

        @app.get("/bg")
        def bg(request: Request, tasks: BackgroundTasks) -> Response:
            out = self._respond(Seen(payer_from(request), is_retry(request), dict(request.headers)))
            tasks.add_task(self.background)
            return Response(out.body, status_code=out.status, headers=out.headers)

        @app.get("/free")
        def free() -> dict[str, str]:
            return {"free": "yes"}

        app.add_middleware(CantonPaymentMiddleware, server=self.server)
        if self.server_kind == "hypercorn":
            self._serve_hypercorn(app)
        else:
            self._serve_uvicorn(app)

    def _serve_uvicorn(self, app: FastAPI) -> None:
        import uvicorn

        config = uvicorn.Config(
            app, host="127.0.0.1", port=0, log_level="error", lifespan="off", http=self.http_impl
        )
        server = uvicorn.Server(config)
        thread = threading.Thread(target=server.run, daemon=True)
        thread.start()
        deadline = time.monotonic() + 10
        while not server.started and time.monotonic() < deadline:
            time.sleep(0.01)
        self.port = server.servers[0].sockets[0].getsockname()[1]

        def stop() -> None:
            server.should_exit = True
            server.force_exit = True
            thread.join(10)

        self._stop = stop

    def _serve_hypercorn(self, app: FastAPI) -> None:
        from hypercorn.asyncio import serve
        from hypercorn.config import Config as HypercornConfig

        port = free_port()
        config = HypercornConfig()
        config.bind = [f"127.0.0.1:{port}"]
        config.graceful_timeout = 1
        config.accesslog = None
        loop = asyncio.new_event_loop()
        stopper: list[asyncio.Event] = []

        def run() -> None:
            asyncio.set_event_loop(loop)

            async def main() -> None:
                stop_event = asyncio.Event()
                stopper.append(stop_event)
                await serve(app, config, shutdown_trigger=stop_event.wait)  # type: ignore[arg-type]

            loop.run_until_complete(main())

        thread = threading.Thread(target=run, daemon=True)
        thread.start()
        wait_for_port(port)
        self.port = port

        def stop() -> None:
            if stopper:
                loop.call_soon_threadsafe(stopper[0].set)
            thread.join(10)

        self._stop = stop

    def _wsgi_app(self) -> Any:
        from flask import Flask, request
        from flask import Response as FlaskResponse

        app = Flask(__name__)

        @app.route("/paid", methods=["GET", "POST"])
        @app.route("/submit", methods=["POST"])
        def paid() -> FlaskResponse:
            out = self._respond(
                Seen(
                    payer_from(),
                    is_retry(),
                    dict(request.headers),
                    request.get_data() if self.read_body else b"",
                )
            )
            response = FlaskResponse(out.body, status=out.status, headers=out.headers)
            if out.cookies:
                response.set_cookie("session", "secret")
            return response

        @app.route("/free")
        def free() -> dict[str, str]:
            return {"free": "yes"}

        app.wsgi_app = CantonWSGIMiddleware(app.wsgi_app, self.server)  # type: ignore[method-assign]
        return app

    def _serve_wsgi(self) -> None:
        app = self._wsgi_app()
        if self.server_kind in ("waitress", "waitress-default"):
            import importlib

            waitress = importlib.import_module("waitress")  # untyped: no stubs needed
            wsgi_server = waitress.create_server(
                app,
                host="127.0.0.1",
                port=0,
                threads=8,
                channel_request_lookahead=0 if self.server_kind == "waitress-default" else 1,
            )
            thread = threading.Thread(target=wsgi_server.run, daemon=True)
            thread.start()
            self.port = wsgi_server.effective_port

            def stop_waitress() -> None:
                wsgi_server.close()
                thread.join(10)

            self._stop = stop_waitress
            return
        if self.server_kind == "wsgiref":
            from wsgiref.simple_server import WSGIRequestHandler, make_server

            class Quiet(WSGIRequestHandler):
                def log_message(self, *args: Any) -> None:
                    pass

            ref = make_server("127.0.0.1", 0, app, handler_class=Quiet)
            thread = threading.Thread(
                target=lambda: ref.serve_forever(poll_interval=0.02), daemon=True
            )
            thread.start()
            self.port = ref.server_port

            def stop_ref() -> None:
                ref.shutdown()
                thread.join(10)

            self._stop = stop_ref
            return
        from werkzeug.serving import make_server as werkzeug_make_server

        server = werkzeug_make_server("127.0.0.1", 0, app, threaded=True)
        thread = threading.Thread(
            target=lambda: server.serve_forever(poll_interval=0.02), daemon=True
        )
        thread.start()
        self.port = server.server_port

        def stop() -> None:
            server.shutdown()
            thread.join(10)

        self._stop = stop

    def _wait_until_marked(self, response: httpx.Response) -> None:
        """A client that replays the moment it has its answer races the delivered mark, which the
        gate writes just after the response (an unmarked claim answers 503, never the final 402).
        Tests that replay wait for the mark."""
        header = response.headers.get("payment-response")
        if header is None:
            return
        try:
            receipt = json.loads(base64.b64decode(header))
        except ValueError:
            return
        if receipt.get("success") is not True:
            return
        store = getattr(self.server.store, "inner", self.server.store)
        update_id = receipt["transaction"]

        def marked() -> bool:
            holder = store.claimant(update_id)
            return bool(holder and holder.startswith("delivered:"))

        wait_until(marked, 3.0, 0.005)

    def close(self) -> None:
        self.client.close()
        self._stop()
        self.server.close()

    # -- requests ---------------------------------------------------------------

    def offer(self, path: str = "/paid") -> dict[str, Any]:
        """The 402's accepted requirements for GET ``path``."""
        if path not in self._offers:
            response = self.client.get(path)
            assert response.status_code == 402, response.text
            header = response.headers["payment-required"]
            self._offers[path] = json.loads(base64.b64decode(header))["accepts"][0]
        return self._offers[path]

    def header(self, n: int = 1, tx: str | None = None, path: str = "/paid", **payload: Any) -> str:
        return payment_header(self.offer(path), tx if tx is not None else tx_hash(n), **payload)

    def pay(
        self, n: int = 1, path: str = "/paid", method: str = "GET", **kwargs: Any
    ) -> httpx.Response:
        headers = {"PAYMENT-SIGNATURE": self.header(n, path=path), **kwargs.pop("headers", {})}
        return self.client.request(method, path, headers=headers, **kwargs)

    def receipt(self, response: httpx.Response) -> dict[str, Any] | None:
        header = response.headers.get("payment-response")
        if header is None:
            return None
        loaded: dict[str, Any] = json.loads(base64.b64decode(header))
        return loaded

    def raw_request(self, request: bytes, hold: float = 0.0) -> bytes:
        """Send raw bytes on a fresh socket, read the answer."""
        with socket.create_connection(("127.0.0.1", self.port), timeout=30) as sock:
            sock.sendall(request)
            if hold:
                time.sleep(hold)
            chunks = []
            while True:
                data = sock.recv(65536)
                if not data:
                    break
                chunks.append(data)
                if b"\r\n\r\n" in b"".join(chunks) and len(b"".join(chunks)) > 0:
                    head, _, body = b"".join(chunks).partition(b"\r\n\r\n")
                    for line in head.split(b"\r\n"):
                        if line.lower().startswith(b"content-length:"):
                            if len(body) >= int(line.split(b":")[1]):
                                return b"".join(chunks)
            return b"".join(chunks)

    def send_and_leave(
        self,
        n: int = 1,
        wait_for_calls: int = 1,
        path: str = "/paid",
        method: str = "GET",
        body: bytes = b"",
    ) -> None:
        """Send a paid request on a raw socket and close it as soon as the facilitator got its first /settle."""
        request = (
            f"{method} {path} HTTP/1.1\r\nHost: x\r\nPAYMENT-SIGNATURE: {self.header(n)}\r\n"
            f"Content-Length: {len(body)}\r\nConnection: close\r\n\r\n"
        ).encode() + body
        before = self.facilitator.count("/settle")
        sock = socket.create_connection(("127.0.0.1", self.port), timeout=30)
        sock.sendall(request)
        deadline = time.monotonic() + 20
        while (
            self.facilitator.count("/settle") < before + wait_for_calls
            and time.monotonic() < deadline
        ):
            time.sleep(0.01)
        sock.shutdown(socket.SHUT_RDWR)
        sock.close()

    def raw(self, request_line: str, extra_headers: str = "") -> tuple[int, dict[str, str], bytes]:
        """One raw HTTP/1.1 request (the client library would normalize the path): status, headers, body."""
        data = self.raw_request(
            f"{request_line} HTTP/1.1\r\nHost: x\r\n{extra_headers}Connection: close\r\n\r\n".encode()
        )
        head, _, body = data.partition(b"\r\n\r\n")
        lines = head.decode("latin-1").split("\r\n")
        headers = {k.lower(): v.strip() for k, _, v in (line.partition(":") for line in lines[1:])}
        return int(lines[0].split()[1]), headers, body


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def wait_for_port(port: int, timeout: float = 10.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.2):
                return
        except OSError:
            time.sleep(0.05)
    raise TimeoutError(f"nothing listens on {port}")


def wait_until(
    condition: Callable[[], bool], timeout: float = 15.0, interval: float = 0.02
) -> bool:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if condition():
            return True
        time.sleep(interval)
    return condition()


def run_async(coro: Any) -> Any:
    return asyncio.run(coro)
