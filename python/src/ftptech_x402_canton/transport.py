"""HTTP transport to the facilitator: never raises for a network problem, never follows redirects."""

from __future__ import annotations

import json
import os
import threading
from dataclasses import dataclass
from typing import Any

import httpx

from . import forksafe

#: The SDK caps facilitator bodies at 1 MiB too.
MAX_RESPONSE_BYTES = 1 << 20
USER_AGENT = "ftptech-x402-canton"


@dataclass(frozen=True)
class HttpAnswer:
    """What came back from one facilitator call."""

    status: int  # 0 when no HTTP answer arrived (transport error, timeout)
    body: Any = None  # parsed JSON, or None when the body was empty, too big or not JSON
    retry_after: float = 0.0  # seconds from Retry-After, 0 when absent
    error: str = ""  # the transport error class, "" when an answer arrived


def parse_retry_after(value: str | None) -> float:
    """The delay-seconds form. The HTTP-date form is ignored: the facilitator sends seconds."""
    if value is None:
        return 0.0
    try:
        seconds = int(value.strip())
    except ValueError:
        return 0.0
    return float(seconds) if seconds > 0 else 0.0


class Transport:
    """The facilitator client, created lazily in each process.

    A pooled connection opened before a fork (gunicorn ``--preload``, uWSGI without
    ``lazy-apps``, a ``multiprocessing`` fork) is inherited by every child, and the
    children would then write to and read from one TCP stream: a worker could read
    another worker's settle answer. So the pool is created on first use in each
    process, and a client opened by another process is never touched again (it is
    dropped without being closed, which would also send on the shared socket).
    A merchant-supplied ``http_client`` is used as given: create it after the fork.
    """

    def __init__(self, base_url: str, client: httpx.Client | None = None) -> None:
        self._base = base_url.rstrip("/")
        self._supplied = client
        self._owned: httpx.Client | None = None
        self.owner_pid = os.getpid()
        self._mutex = threading.Lock()
        self._abandoned: list[httpx.Client] = []
        forksafe.register(self)

    def reset_after_fork(self) -> None:
        self._mutex = threading.Lock()

    @property
    def client(self) -> httpx.Client:
        if self._supplied is not None:
            return self._supplied
        pid = os.getpid()
        with self._mutex:
            if self._owned is not None and self.owner_pid != pid:
                # Opened before a fork: shared with the parent. Keep it referenced (a
                # garbage-collected client would close the shared sockets) and never use it.
                self._abandoned.append(self._owned)
                self._owned = None
            if self._owned is None:
                # follow_redirects stays off: a redirected POST /settle is not a verdict.
                self._owned = httpx.Client(
                    follow_redirects=False,
                    limits=httpx.Limits(max_connections=256, max_keepalive_connections=32),
                )
                self.owner_pid = pid
            return self._owned

    def close(self) -> None:
        with self._mutex:
            owned, self._owned = self._owned, None
        if owned is not None and self.owner_pid == os.getpid():
            owned.close()

    def get(self, path: str, timeout: float) -> HttpAnswer:
        return self._send("GET", path, None, timeout)

    def post(self, path: str, body: bytes, timeout: float) -> HttpAnswer:
        return self._send("POST", path, body, timeout)

    def _send(self, method: str, path: str, body: bytes | None, timeout: float) -> HttpAnswer:
        headers = {"User-Agent": USER_AGENT, "Accept": "application/json"}
        if body is not None:
            headers["Content-Type"] = "application/json"
        try:
            # timeout is per phase (connect, write, read): a facilitator that
            # answers after a long wait is fine, one that stalls is cut off.
            with self.client.stream(
                method,
                self._base + path,
                content=body,
                headers=headers,
                timeout=httpx.Timeout(timeout),
                follow_redirects=False,
            ) as response:
                raw = _read_capped(response)
                retry_after = parse_retry_after(response.headers.get("Retry-After"))
                return HttpAnswer(response.status_code, _parse_json(raw), retry_after)
        except Exception as error:
            # Any failure of the call is "no answer": the outcome is unknown. A closed
            # client (shutdown between two calls) is one of them.
            return HttpAnswer(0, error=type(error).__name__)


def _read_capped(response: httpx.Response) -> bytes | None:
    chunks: list[bytes] = []
    size = 0
    for chunk in response.iter_bytes():
        size += len(chunk)
        if size > MAX_RESPONSE_BYTES:
            return None
        chunks.append(chunk)
    return b"".join(chunks)


def _parse_json(raw: bytes | None) -> Any:
    if not raw:
        return None
    try:
        return json.loads(raw)
    except ValueError:
        return None
