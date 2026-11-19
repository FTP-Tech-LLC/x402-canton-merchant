"""A mock facilitator that answers like ``settle.ts`` and ``verify.ts`` in spec mode.

Scriptable per call: queue answers for /settle or /verify, or set a mode. Every
request is recorded with its exact bytes.
"""

from __future__ import annotations

import json
import threading
import time
from collections import deque
from collections.abc import Callable
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

DSO = "DSO::1220b1431ef217342db44d516bb9befde802be7d8899637d290895fa58880f19accc"
FEE_PAYER = "ftp-x402-infrastructure::1220" + "da" * 32
SYNCHRONIZER = "global-domain::1220" + "b1" * 32
PAYER = "agent::1220" + "cc" * 32
NETWORK = "canton:mainnet"


def default_supported() -> dict[str, Any]:
    return {
        "kinds": [
            {
                "x402Version": 2,
                "scheme": "exact",
                "network": NETWORK,
                "extra": {
                    "transferMethods": ["transfer-factory"],
                    "feePayer": FEE_PAYER,
                    "synchronizerId": SYNCHRONIZER,
                    "instruments": [
                        {"instrumentId": {"admin": DSO, "id": "Amulet"}, "symbol": "CC"}
                    ],
                },
            }
        ],
        "extensions": [],
        "signers": {"canton:*": [FEE_PAYER]},
    }


@dataclass
class Answer:
    """One scripted answer."""

    status: int = 200
    body: Any = None
    headers: dict[str, str] = field(default_factory=dict)
    delay: float = 0.0
    commit: bool = (
        False  # the payment settles on the ledger even though this answer is not the verdict
    )
    raw: bytes | None = None  # send these bytes instead of JSON
    drop: bool = False  # close the connection without answering
    updates: bool = True


@dataclass
class Call:
    path: str
    body: bytes
    headers: dict[str, str]
    at: float

    def json(self) -> dict[str, Any]:
        loaded: dict[str, Any] = json.loads(self.body)
        return loaded

    @property
    def hash(self) -> str:
        return str(self.json()["paymentPayload"]["payload"]["preparedTxHash"]).lower().strip()


def pending(transaction: str = "", delay: float = 0.0, commit: bool = False) -> Answer:
    return Answer(
        503,
        {
            "success": False,
            "errorReason": "settlement_pending",
            "transaction": transaction,
            "network": NETWORK,
        },
        {"Retry-After": "3"},
        delay,
        commit,
    )


def refuse(reason: str = "invalid_exact_canton_expired", status: int = 200) -> Answer:
    return Answer(
        status, {"success": False, "errorReason": reason, "transaction": "", "network": NETWORK}
    )


def bare(status: int, retry_after: str | None = None) -> Answer:
    return Answer(status, None, {"Retry-After": retry_after} if retry_after else {}, raw=b"")


class MockFacilitator:
    def __init__(self) -> None:
        self.supported: Any = default_supported()
        self.supported_queue: deque[Answer] = deque()
        self.settle_queue: deque[Answer] = deque()
        self.verify_queue: deque[Answer] = deque()
        self.settle_always: Answer | None = None  # when set, every /settle gets this
        self.legacy = False  # settleAnswers "legacy": a repeat is a success
        self.payer = PAYER
        self.settled: dict[str, str] = {}
        self.calls: list[Call] = []
        self._lock = threading.Lock()
        self._server: ThreadingHTTPServer | None = None
        self.url = ""
        self.on_call: Callable[[Call], None] | None = None
        self.verify_expired = (
            False  # /verify answers "expired" for everything (the payment deadline passed)
        )

    # -- lifecycle ------------------------------------------------------------

    def start(self) -> MockFacilitator:
        outer = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *args: Any) -> None:
                pass

            def do_GET(self) -> None:
                outer._handle(self, b"")

            def do_POST(self) -> None:
                length = int(self.headers.get("Content-Length") or 0)
                outer._handle(self, self.rfile.read(length))

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        server.daemon_threads = True
        self._server = server
        threading.Thread(
            target=lambda: server.serve_forever(poll_interval=0.02), daemon=True
        ).start()
        self.url = f"http://127.0.0.1:{server.server_port}"
        return self

    def stop(self) -> None:
        if self._server is not None:
            self._server.shutdown()
            self._server.server_close()

    # -- views ----------------------------------------------------------------

    def paths(self) -> list[str]:
        return [c.path for c in self.calls]

    def count(self, path: str) -> int:
        return sum(1 for c in self.calls if c.path == path)

    def calls_to(self, path: str) -> list[Call]:
        return [c for c in self.calls if c.path == path]

    def update_id(self, tx_hash: str) -> str:
        return "1220u" + tx_hash[-60:]

    # -- handling ---------------------------------------------------------------

    def _handle(self, request: BaseHTTPRequestHandler, body: bytes) -> None:
        path = request.path.split("?")[0]
        with self._lock:
            self.calls.append(
                Call(
                    path, body, {k.lower(): v for k, v in request.headers.items()}, time.monotonic()
                )
            )
        if self.on_call is not None:
            self.on_call(self.calls[-1])
        if path == "/supported":
            answer = (
                self.supported_queue.popleft()
                if self.supported_queue
                else Answer(200, self.supported)
            )
        elif path == "/verify":
            answer = self.verify_queue.popleft() if self.verify_queue else self._default_verify()
        elif path == "/settle":
            answer = self._next_settle(body)
        else:
            answer = Answer(404, {"error": "not_found"})
        self._send(request, answer)

    def _default_verify(self) -> Answer:
        if self.verify_expired:
            return Answer(
                200,
                {
                    "isValid": False,
                    "invalidReason": "invalid_exact_canton_expired",
                    "payer": self.payer,
                },
            )
        return Answer(200, {"isValid": True, "payer": self.payer})

    def _next_settle(self, body: bytes) -> Answer:
        tx_hash = json.loads(body)["paymentPayload"]["payload"]["preparedTxHash"].lower().strip()
        with self._lock:
            scripted = self.settle_always or (
                self.settle_queue.popleft() if self.settle_queue else None
            )
            if scripted is not None:
                if scripted.commit:
                    self.settled.setdefault(tx_hash, self.update_id(tx_hash))
                return scripted
            return self._default_settle(tx_hash)

    def _default_settle(self, tx_hash: str) -> Answer:
        if tx_hash in self.settled:
            update_id = self.settled[tx_hash]
            if self.legacy:
                return Answer(
                    200,
                    {
                        "success": True,
                        "transaction": update_id,
                        "network": NETWORK,
                        "payer": self.payer,
                    },
                )
            return Answer(
                200,
                {
                    "success": False,
                    "errorReason": "invalid_exact_canton_payment_already_settled",
                    "transaction": update_id,
                    "network": NETWORK,
                    "payer": self.payer,
                },
            )
        update_id = self.update_id(tx_hash)
        self.settled[tx_hash] = update_id
        return Answer(
            200,
            {"success": True, "transaction": update_id, "network": NETWORK, "payer": self.payer},
        )

    def _send(self, request: BaseHTTPRequestHandler, answer: Answer) -> None:
        if answer.delay:
            time.sleep(answer.delay)
        if answer.drop:
            request.close_connection = True
            request.connection.close()
            return
        payload = answer.raw if answer.raw is not None else json.dumps(answer.body).encode()
        try:
            request.send_response(answer.status)
            request.send_header("Content-Type", "application/json")
            request.send_header("Content-Length", str(len(payload)))
            for name, value in answer.headers.items():
                request.send_header(name, value)
            request.end_headers()
            request.wfile.write(payload)
        except OSError:
            pass
