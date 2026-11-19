"""Regressions from the Python audit (runtime lens): servers, forks, bodies, shutdown."""

from __future__ import annotations

import base64
import json
import logging
import os
import select
import socket
import ssl
import sys
import threading
import time
from collections.abc import Callable
from typing import Any

import httpx
import pytest

from ftptech_x402_canton import Route, cc
from ftptech_x402_canton.framing import framed
from ftptech_x402_canton.responses import GateResponse
from ftptech_x402_canton.transport import Transport
from ftptech_x402_canton.wsgi import CountingInput, client_gone, drain_input, socket_gone
from tests import inproc
from tests.mock_facilitator import PAYER, Answer, MockFacilitator
from tests.support import PAY_TO, Harness, Out, Seen, tx_hash, wait_until


def slow_success(facilitator: MockFacilitator, n: int = 1, delay: float = 0.6) -> None:
    facilitator.settle_queue.append(
        Answer(
            200,
            {
                "success": True,
                "transaction": facilitator.update_id(tx_hash(n)),
                "network": "canton:mainnet",
                "payer": PAYER,
            },
            delay=delay,
        )
    )


def post_offer(harness: Harness, path: str = "/paid") -> dict[str, Any]:
    response = harness.client.post(path)
    assert response.status_code == 402
    accepted: dict[str, Any] = json.loads(base64.b64decode(response.headers["payment-required"]))[
        "accepts"
    ][0]
    return accepted


POST_ROUTES = {"POST /paid": Route(pay_to=PAY_TO, price=cc("0.01"))}


# -- the facilitator client is per process (audit runtime 1) --------------------------------------------


def test_the_transport_client_is_created_per_process_and_never_shared_across_a_fork(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    transport = Transport("http://127.0.0.1:1")
    parent = transport.client
    assert transport.client is parent and transport.owner_pid == os.getpid()
    monkeypatch.setattr(
        os,
        "getpid",
        lambda: os.getpid.__wrapped__() + 1 if hasattr(os.getpid, "__wrapped__") else 4242,
    )
    child = transport.client  # "after a fork": another pid
    assert child is not parent and transport.owner_pid != 0
    assert (
        not parent.is_closed
    )  # the parent's pool is not touched (closing would write to shared sockets)
    assert transport.client is child
    monkeypatch.undo()


def test_a_supplied_http_client_is_used_as_given() -> None:
    mine = httpx.Client()
    assert Transport("http://x", mine).client is mine
    mine.close()


@pytest.mark.skipif(not hasattr(os, "fork"), reason="needs fork")
@pytest.mark.filterwarnings(
    "ignore:.*fork.*:DeprecationWarning"
)  # the child only reads one attribute
def test_a_real_fork_gets_its_own_client() -> None:
    transport = Transport("http://127.0.0.1:1")
    parent = transport.client
    read_end, write_end = os.pipe()
    pid = os.fork()
    if pid == 0:  # the child
        try:
            os.close(read_end)
            child = transport.client
            os.write(
                write_end,
                b"1" if child is not parent and transport.owner_pid == os.getpid() else b"0",
            )
        finally:
            os._exit(0)
    os.close(write_end)
    assert os.read(read_end, 1) == b"1"
    os.waitpid(pid, 0)
    os.close(read_end)
    assert transport.owner_pid == os.getpid()


# -- wsgi.input: never re-read what the handler read (audit runtime 3) ---------------------------------------


@pytest.mark.parametrize("server", ["default", "wsgiref"])
def test_a_paid_post_whose_handler_reads_its_body_completes(
    make: Callable[..., Harness], facilitator: MockFacilitator, server: str, kind: str
) -> None:
    if kind != "wsgi":
        pytest.skip("WSGI servers only")
    harness = make(routes=POST_ROUTES, server=server)
    harness.read_body = True
    harness._offers["/paid"] = post_offer(harness)  # noqa: SLF001
    body = b"x" * 5000
    started = time.monotonic()
    response = harness.client.post(
        "/paid", content=body, headers={"PAYMENT-SIGNATURE": harness.header(1)}, timeout=10
    )
    assert response.status_code == 200 and time.monotonic() - started < 5
    assert harness.seen[0].body == body
    assert facilitator.count("/settle") == 1


def test_a_paid_post_whose_handler_reads_part_of_its_body_drains_only_the_rest(
    make: Callable[..., Harness], kind: str
) -> None:
    if kind != "wsgi":
        pytest.skip("WSGI servers only")
    harness = make(routes=POST_ROUTES)
    harness._offers["/paid"] = post_offer(harness)  # noqa: SLF001
    response = harness.client.post(
        "/paid", content=b"y" * 3000, headers={"PAYMENT-SIGNATURE": harness.header(1)}, timeout=10
    )
    assert (
        response.status_code == 200
    )  # the unread body was drained to its declared end, no further


def test_drain_only_reads_what_the_handler_left() -> None:
    class Stream:
        def __init__(self, size: int) -> None:
            self.left = size
            self.asked: list[int] = []

        def read(self, size: int = -1) -> bytes:
            self.asked.append(size)
            if self.left <= 0:
                raise AssertionError("read past the declared body")
            take = min(size, self.left)
            self.left -= take
            return b"z" * take

        def readline(self, size: int = -1) -> bytes:
            return self.read(min(size, 10) if size > 0 else 10)

    raw = Stream(1000)
    environ: dict[str, Any] = {"CONTENT_LENGTH": "1000", "wsgi.input": CountingInput(raw)}
    environ["ftptech_x402_canton.input"] = environ["wsgi.input"]
    environ["wsgi.input"].read(1000)  # the handler read it all
    drain_input(environ)  # must not read again: the raw stream raises if it does
    assert raw.left == 0
    partial = Stream(1000)
    environ2: dict[str, Any] = {"CONTENT_LENGTH": "1000", "wsgi.input": CountingInput(partial)}
    environ2["ftptech_x402_canton.input"] = environ2["wsgi.input"]
    environ2["wsgi.input"].read(400)
    drain_input(environ2)
    assert partial.left == 0 and sum(partial.asked[1:]) == 600  # exactly the remaining 600


def test_counting_input_counts_every_way_of_reading() -> None:
    import io

    counter = CountingInput(io.BytesIO(b"abc\ndef\nghi\njkl"))
    assert counter.read(2) == b"ab" and counter.readline() == b"c\n"
    assert counter.readlines() == [b"def\n", b"ghi\n", b"jkl"]
    assert counter.consumed == 15
    again = CountingInput(io.BytesIO(b"1\n2\n"))
    assert list(again) == [b"1\n", b"2\n"] and again.consumed == 4


def test_chunked_drain_only_where_the_server_terminates_the_body() -> None:
    import io

    unterminated = io.BytesIO(b"abcdef")
    drain_input({"wsgi.input": unterminated})  # no CONTENT_LENGTH, not terminated: left alone
    assert unterminated.read() == b"abcdef"
    terminated = io.BytesIO(b"abcdef")
    drain_input({"wsgi.input": terminated, "wsgi.input_terminated": True})
    assert terminated.read() == b""


# -- detecting a payer who left (audit runtime 2) ---------------------------------------------------------------


def test_waitress_payer_leaves_during_settle_then_the_retry_is_served_once(
    facilitator: MockFacilitator,
) -> None:
    harness = Harness("wsgi", facilitator, server="waitress")
    try:
        slow_success(facilitator)
        harness.send_and_leave(1)
        assert wait_until(lambda: len(harness.server.registry) == 0, 10)
        assert harness.runs == 1
        retry = harness.pay(1)
        assert retry.status_code == 200, retry.text  # charged once, and served on the retry
        assert facilitator.count("/settle") == 1
        assert harness.pay(1).status_code == 402
    finally:
        harness.close()


def test_waitress_payer_gone_before_settle_is_not_charged(facilitator: MockFacilitator) -> None:
    """The handler is slow and the payer leaves: with ``channel_request_lookahead=1`` waitress says so
    (``waitress.client_disconnected``), and nothing is settled."""
    harness = Harness("wsgi", facilitator, server="waitress")
    try:

        def slow(seen: Seen) -> Out:
            time.sleep(0.8)
            return Out()

        harness.handler = slow
        request = (
            f"GET /paid HTTP/1.1\r\nHost: x\r\nPAYMENT-SIGNATURE: {harness.header(1)}\r\nConnection: close\r\n\r\n"
        ).encode()
        sock = socket.create_connection(("127.0.0.1", harness.port), timeout=10)
        sock.sendall(request)
        assert wait_until(lambda: harness.runs == 1, 10)
        sock.shutdown(socket.SHUT_RDWR)
        sock.close()
        assert wait_until(lambda: len(harness.server.registry) == 0, 10)
        assert facilitator.count("/settle") == 0  # not charged
        harness.handler = lambda seen: Out()
        assert harness.pay(1).status_code == 200
    finally:
        harness.close()


def test_client_gone_uses_the_waitress_signal(monkeypatch: pytest.MonkeyPatch) -> None:
    assert client_gone({"waitress.client_disconnected": lambda: True}) is True
    assert client_gone({"waitress.client_disconnected": lambda: False}) is False


def test_client_gone_uses_uwsgi_when_it_is_the_server(monkeypatch: pytest.MonkeyPatch) -> None:
    import types

    fake = types.ModuleType("uwsgi")
    fake.connection_fd = lambda: 7  # type: ignore[attr-defined]
    state = {"connected": True}
    fake.is_connected = lambda fd: state["connected"]  # type: ignore[attr-defined]
    monkeypatch.setitem(sys.modules, "uwsgi", fake)
    environ = {"uwsgi.version": b"2.0"}
    assert client_gone(environ) is False
    state["connected"] = False
    assert client_gone(environ) is True


def test_without_any_signal_the_gate_warns_once_and_names_the_consequence(
    monkeypatch: pytest.MonkeyPatch, caplog: pytest.LogCaptureFixture
) -> None:
    import ftptech_x402_canton.wsgi as wsgi_module

    monkeypatch.setattr(wsgi_module, "_warned", set())
    assert client_gone({}) is False and client_gone({}) is False
    warnings = [r for r in caplog.records if "no way to see a payer who left" in r.getMessage()]
    assert len(warnings) == 1 and warnings[0].levelno == logging.ERROR
    assert (
        "payment_already_redeemed" in warnings[0].getMessage()
        and "waitress" in warnings[0].getMessage()
    )


def test_a_tls_socket_is_unknown_not_gone_and_says_so_loudly(
    monkeypatch: pytest.MonkeyPatch, caplog: pytest.LogCaptureFixture
) -> None:
    import ftptech_x402_canton.wsgi as wsgi_module

    monkeypatch.setattr(wsgi_module, "_warned", set())
    left, right = socket.socketpair()
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    tls = context.wrap_socket(left, server_hostname="x", do_handshake_on_connect=False)
    try:
        assert client_gone({"gunicorn.socket": tls}) is False
        assert client_gone({"gunicorn.socket": tls}) is False
    finally:
        tls.close()
        right.close()
    warnings = [r for r in caplog.records if "TLS ends in the WSGI server" in r.getMessage()]
    assert len(warnings) == 1 and "payment_already_redeemed" in warnings[0].getMessage()


def test_the_socket_peek_sees_eof_and_ignores_pending_data() -> None:
    left, right = socket.socketpair()
    try:
        assert socket_gone(left) is False  # nothing to read: connected
        right.sendall(b"pipelined")
        assert socket_gone(left) is False  # data, not EOF
        right.close()
        left.recv(100)
        assert socket_gone(left) is True
    finally:
        left.close()


def test_the_socket_peek_does_not_open_a_descriptor_and_never_calls_a_failed_poll_gone(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    left, right = socket.socketpair()
    try:
        before = len(os.listdir("/proc/self/fd")) if os.path.isdir("/proc/self/fd") else 0
        for _ in range(50):
            socket_gone(left)
        after = len(os.listdir("/proc/self/fd")) if os.path.isdir("/proc/self/fd") else 0
        assert after == before  # an epoll descriptor per check would fail at the fd limit

        def emfile(*args: Any) -> Any:
            raise OSError(24, "Too many open files")

        monkeypatch.setattr(select, "poll", emfile, raising=False)
        monkeypatch.setattr(select, "select", emfile)
        assert socket_gone(left) is False  # a failing check is "unknown", never "gone"
    finally:
        left.close()
        right.close()


# -- unread request bodies (audit runtime 4) ------------------------------------------------------------------------------


@pytest.mark.parametrize("server", ["uvicorn", "hypercorn"])
def test_an_unread_large_body_does_not_block_or_hang_an_asgi_paid_route(
    facilitator: MockFacilitator, server: str
) -> None:
    harness = Harness("asgi", facilitator, routes=POST_ROUTES, server=server)
    try:
        harness._offers["/paid"] = post_offer(harness)  # noqa: SLF001
        body = b"u" * (3 << 20)  # far more than the pump queues and the drain used to read
        started = time.monotonic()
        response = harness.client.post(
            "/paid", content=body, headers={"PAYMENT-SIGNATURE": harness.header(1)}, timeout=20
        )
        assert response.status_code == 200, response.text
        assert time.monotonic() - started < 15
    finally:
        harness.close()


@pytest.mark.parametrize("server", ["uvicorn", "hypercorn"])
def test_a_payer_who_leaves_with_a_large_unread_body_is_served_on_retry(
    facilitator: MockFacilitator, server: str
) -> None:
    harness = Harness("asgi", facilitator, routes=POST_ROUTES, server=server)
    try:
        harness._offers["/paid"] = post_offer(harness)  # noqa: SLF001
        slow_success(facilitator, 1, 0.8)
        harness.send_and_leave(1, method="POST", body=b"u" * (1 << 20))
        assert wait_until(lambda: len(harness.server.registry) == 0, 15)
        retry = harness.client.post(
            "/paid", content=b"y", headers={"PAYMENT-SIGNATURE": harness.header(1)}, timeout=20
        )
        assert retry.status_code == 200, retry.text  # not the final 402: the body was never claimed
        assert facilitator.count("/settle") == 1
    finally:
        harness.close()


def test_a_body_that_never_finishes_is_treated_as_a_payer_who_left(
    facilitator: MockFacilitator,
) -> None:
    harness = Harness("asgi", facilitator, routes=POST_ROUTES, body_drain_timeout=0.5)
    try:
        harness._offers["/paid"] = post_offer(harness)  # noqa: SLF001
        request = (
            f"POST /paid HTTP/1.1\r\nHost: x\r\nPAYMENT-SIGNATURE: {harness.header(1)}\r\n"
            "Content-Length: 100\r\nConnection: close\r\n\r\nonly ten b"
        ).encode()
        with socket.create_connection(("127.0.0.1", harness.port), timeout=20) as sock:
            sock.sendall(request)
            deadline = time.monotonic() + 10
            data = b""
            while b"\r\n\r\n" not in data and time.monotonic() < deadline:
                data += sock.recv(4096)
        assert data.startswith(b"HTTP/1.1 503")  # nothing was charged: the payment was not settled
        assert facilitator.count("/settle") == 0
    finally:
        harness.close()


# -- BackgroundTasks (audit runtime 6) -----------------------------------------------------------------------------------------


def test_a_background_task_runs_alongside_the_settle_not_before_it(
    facilitator: MockFacilitator,
) -> None:
    routes = {"GET /bg": Route(pay_to=PAY_TO, price=cc("0.01"))}
    harness = Harness("asgi", facilitator, routes=routes)
    finished = threading.Event()
    settles_when_done: list[int] = []

    def background() -> None:
        time.sleep(1.0)
        settles_when_done.append(facilitator.count("/settle"))
        finished.set()

    harness.background = background
    try:
        started = time.monotonic()
        response = harness.pay(1, path="/bg")
        elapsed = time.monotonic() - started
        assert response.status_code == 200 and elapsed < 0.7, (
            elapsed
        )  # the task does not delay the paid answer
        assert facilitator.count("/settle") == 1
        assert finished.wait(10) and settles_when_done == [
            1
        ]  # it still ran, after the settle began
    finally:
        harness.close()


# -- HEAD answers carry no body (audit runtime 11) ---------------------------------------------------------------------------------


def test_the_answer_to_a_head_has_no_body_but_keeps_its_content_length() -> None:
    response = GateResponse(402, [("Content-Type", "application/json")], b'{"error":"x"}')
    headers, body = framed(response, "HEAD")
    assert body == b"" and ("Content-Length", "13") in headers
    headers_get, body_get = framed(response, "GET")
    assert body_get == b'{"error":"x"}' and ("Content-Length", "13") in headers_get


def test_a_wsgi_head_never_writes_a_body(facilitator: MockFacilitator) -> None:
    server = inproc.build_server(facilitator)
    written: list[bytes] = []
    status: list[str] = []

    def start_response(s: str, h: list[tuple[str, str]], e: Any = None) -> Callable[[bytes], None]:
        status.append(s)
        return written.append

    environ = {
        "REQUEST_METHOD": "HEAD", "PATH_INFO": "/paid", "SCRIPT_NAME": "", "QUERY_STRING": "",
        "SERVER_NAME": "x", "wsgi.url_scheme": "http", "HTTP_HOST": "x",
    }  # fmt: skip
    from ftptech_x402_canton import CantonWSGIMiddleware

    CantonWSGIMiddleware(inproc.wsgi_app("200 OK"), server)(environ, start_response)
    assert status[0].startswith("402") and written == []
    server.close()


# -- admission and shutdown (audit runtime 7, 8) --------------------------------------------------------------------------------------


def test_a_full_pool_answers_503_busy_at_once_instead_of_queueing(
    make: Callable[..., Harness], facilitator: MockFacilitator, kind: str
) -> None:
    if kind != "asgi":
        pytest.skip("the thread pool is the ASGI driver's")
    harness = make(worker_threads=1)
    slow_success(facilitator, 1, 1.0)
    harness.offer()
    first: list[int] = []
    thread = threading.Thread(target=lambda: first.append(harness.pay(1).status_code), daemon=True)
    thread.start()
    assert wait_until(lambda: facilitator.count("/settle") == 1, 10)
    started = time.monotonic()
    busy = harness.pay(2)
    assert busy.status_code == 503 and time.monotonic() - started < 0.5
    assert busy.json() == {
        "error": "payment service is busy: if you already paid, do NOT pay again; retry the same payment"
    }
    assert busy.headers["retry-after"] == "3"
    assert (
        facilitator.count("/verify") == 1 and facilitator.count("/settle") == 1
    )  # nothing was done for it
    thread.join(10)
    assert first == [200]
    assert harness.pay(2).status_code == 200  # capacity is back


def test_the_pool_is_not_recreated_after_close(make: Callable[..., Harness], kind: str) -> None:
    harness = make()
    harness.server.close()
    with pytest.raises(Exception, match="closed"):
        _ = harness.server.executor
    harness.server.close()  # idempotent


def test_a_probe_of_logging_is_available(caplog: pytest.LogCaptureFixture) -> None:
    logging.getLogger("ftptech_x402_canton").warning("x")
    assert caplog.records and Seen is not None and Out is not None
