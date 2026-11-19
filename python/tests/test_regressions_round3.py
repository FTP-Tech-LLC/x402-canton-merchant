"""Regressions from round 3 of the Python audit."""

from __future__ import annotations

import asyncio
import base64
import io
import json
import logging
import os
import select
import socket
import subprocess
import sys
import threading
import time
from collections.abc import Callable, Iterator
from pathlib import Path
from typing import Any

import httpx
import pytest

from ftptech_x402_canton import Route, cc
from ftptech_x402_canton.ledger import Ledger
from ftptech_x402_canton.wsgi import CountingInput, socket_gone
from tests import inproc
from tests.mock_facilitator import MockFacilitator, refuse
from tests.stores import SpyStore
from tests.support import PAY_TO, Harness, Out, Seen, free_port, payment_header, tx_hash, wait_until
from tests.test_regressions_round2 import UWSGI, ReplayingLockStore

POST = {"POST /paid": Route(pay_to=PAY_TO, price=cc("0.01"))}


def post_offer(harness: Harness) -> dict[str, Any]:
    response = harness.client.post("/paid")
    assert response.status_code == 402
    accepted: dict[str, Any] = json.loads(base64.b64decode(response.headers["payment-required"]))[
        "accepts"
    ][0]
    return accepted


# -- CountingInput offers only what the wrapped input offers (runtime high) -------------------------------------


def test_counting_input_hides_readinto_and_read1_when_the_stream_has_none() -> None:
    class Plain:
        def read(self, size: int = -1) -> bytes:
            return b""

    bare = CountingInput(Plain())
    assert not hasattr(bare, "readinto") and not hasattr(
        bare, "read1"
    )  # werkzeug then falls back to read()
    full = CountingInput(io.BytesIO(b"abcdef"))
    buffer = bytearray(4)
    assert hasattr(full, "readinto") and full.readinto(buffer) == 4 and full.consumed == 4
    assert full.read1(2) == b"ef" and full.consumed == 6


def start_server(
    facilitator: MockFacilitator, *argv: str, port: int | None = None
) -> Iterator[int]:
    port = port or free_port()
    env = {**os.environ, "FACILITATOR_URL": facilitator.url, "PYTHONPATH": os.getcwd()}
    process = subprocess.Popen(
        [
            sys.executable,
            "-m",
            *argv[0].split(),
            *[a.replace("{port}", str(port)) for a in argv[1:]],
        ],
        env=env,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    try:
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            try:
                with socket.create_connection(("127.0.0.1", port), timeout=0.3):
                    break
            except OSError:
                time.sleep(0.2)
        else:
            pytest.skip("server did not start")
        yield port
    finally:
        process.terminate()
        process.wait(15)


def offer_post(port: int) -> dict[str, Any]:
    response = httpx.post(f"http://127.0.0.1:{port}/post", json={})
    assert response.status_code == 402
    accepted: dict[str, Any] = json.loads(base64.b64decode(response.headers["payment-required"]))[
        "accepts"
    ][0]
    return accepted


def test_a_paid_post_that_reads_its_json_works_on_gunicorn_with_max_content_length(
    facilitator: MockFacilitator,
) -> None:
    for port in start_server(
        facilitator,
        "gunicorn tests.gunicorn_app:app",
        "--bind",
        "127.0.0.1:{port}",
        "--workers",
        "1",
        "--log-level",
        "critical",
    ):
        accepted = offer_post(port)
        response = httpx.post(
            f"http://127.0.0.1:{port}/post",
            json={"a": 1},
            headers={"PAYMENT-SIGNATURE": payment_header(accepted, tx_hash(1))},
            timeout=20,
        )
        assert response.status_code == 200, (
            response.text
        )  # werkzeug used to call a readinto gunicorn's input lacks
        assert response.json() == {"got": {"a": 1}}


@pytest.mark.skipif(not UWSGI.exists(), reason="pyuwsgi needed")
def test_a_paid_post_that_reads_its_json_works_on_real_uwsgi(facilitator: MockFacilitator) -> None:
    port = free_port()
    env = {**os.environ, "FACILITATOR_URL": facilitator.url, "PYTHONPATH": os.getcwd()}
    process = subprocess.Popen(
        [str(UWSGI), "--http-socket", f"127.0.0.1:{port}", "--module", "tests.gunicorn_app:app", "--lazy-apps",
         "--master", "--processes", "1", "--enable-threads", "--buffer-size", "32768", "--disable-logging"],
        env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )  # fmt: skip
    try:
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            try:
                with socket.create_connection(("127.0.0.1", port), timeout=0.3):
                    break
            except OSError:
                time.sleep(0.2)
        else:
            pytest.skip("uwsgi did not start")
        accepted = offer_post(port)
        response = httpx.post(
            f"http://127.0.0.1:{port}/post",
            json={"a": 2},
            headers={"PAYMENT-SIGNATURE": payment_header(accepted, tx_hash(2))},
            timeout=20,
        )
        assert response.status_code == 200, response.text
        assert response.json() == {"got": {"a": 2}}
    finally:
        process.terminate()
        process.wait(15)


# -- the fork hook survives a fork (money and runtime medium) ----------------------------------------------------


@pytest.mark.skipif(not hasattr(os, "fork"), reason="needs fork")
@pytest.mark.filterwarnings("ignore:.*fork.*:DeprecationWarning")
def test_a_forked_worker_still_starts_its_reconciler_and_checks_its_store(
    facilitator: MockFacilitator,
) -> None:
    server = inproc.build_server(facilitator)
    read_end, write_end = os.pipe()
    pid = os.fork()
    if pid == 0:  # the child: a worker
        try:
            os.close(read_end)
            hooked = server.gate.on_paid_request is not None
            if hooked:
                server.gate.on_paid_request()  # type: ignore[misc]
            deadline = time.monotonic() + 3
            while not server.reconciler.running and time.monotonic() < deadline:
                time.sleep(0.01)
            os.write(write_end, b"1" if hooked and server.reconciler.running else b"0")
        finally:
            os._exit(0)
    os.close(write_end)
    assert os.read(read_end, 1) == b"1"
    os.waitpid(pid, 0)
    os.close(read_end)
    server.close()


# -- the drain waits for a body that is still arriving (runtime medium) -----------------------------------------------


def test_a_slow_upload_the_handler_ignores_is_waited_for_then_settled(
    make: Callable[..., Harness], facilitator: MockFacilitator, kind: str
) -> None:
    if kind != "wsgi":
        pytest.skip("WSGI only")
    harness = make(routes=POST, body_drain_timeout=10)
    harness._offers["/paid"] = post_offer(harness)  # noqa: SLF001
    body = b"s" * 200_000
    head = (
        f"POST /paid HTTP/1.1\r\nHost: x\r\nPAYMENT-SIGNATURE: {harness.header(1)}\r\nContent-Length: {len(body)}\r\nConnection: close\r\n\r\n"
    ).encode()
    with socket.create_connection(("127.0.0.1", harness.port), timeout=20) as sock:
        sock.sendall(head + body[:50_000])
        assert wait_until(lambda: harness.runs == 1, 10)  # the handler ran and left the body
        time.sleep(0.5)  # the rest is still coming: the gate must not settle yet
        assert facilitator.count("/settle") == 0
        sock.sendall(body[50_000:])
        data = b""
        while b"\r\n\r\n" not in data:
            data += sock.recv(4096)
    assert data.startswith(b"HTTP/1.1 200") and facilitator.count("/settle") == 1


def test_an_upload_that_never_finishes_is_a_payer_who_left_not_a_charge(
    make: Callable[..., Harness], facilitator: MockFacilitator, kind: str
) -> None:
    if kind != "wsgi":
        pytest.skip("WSGI only")
    harness = make(routes=POST, body_drain_timeout=0.6)
    harness._offers["/paid"] = post_offer(harness)  # noqa: SLF001
    head = (
        f"POST /paid HTTP/1.1\r\nHost: x\r\nPAYMENT-SIGNATURE: {harness.header(1)}\r\nContent-Length: 200000\r\nConnection: close\r\n\r\n"
    ).encode()
    with socket.create_connection(("127.0.0.1", harness.port), timeout=20) as sock:
        sock.sendall(head + b"x" * 1000)
        data = b""
        while b"\r\n\r\n" not in data:
            data += sock.recv(4096)
    assert data.startswith(b"HTTP/1.1 503")
    assert facilitator.count("/settle") == 0  # nothing was charged


# -- busy slots, locks, refusals (money lows) -------------------------------------------------------------------------


def test_the_busy_slot_is_free_while_a_background_task_runs(facilitator: MockFacilitator) -> None:
    routes = {"GET /bg": Route(pay_to=PAY_TO, price=cc("0.01"))}
    harness = Harness("asgi", facilitator, routes=routes, worker_threads=1)
    finished = threading.Event()

    def background() -> None:
        time.sleep(2.0)
        finished.set()

    harness.background = background
    try:
        assert harness.pay(1, path="/bg").status_code == 200
        second = harness.pay(2, path="/bg")  # the first one's background work uses no worker thread
        assert second.status_code == 200, second.text
    finally:
        finished.wait(10)
        harness.close()


def test_a_failed_lock_read_back_releases_the_lock_it_may_hold() -> None:
    spy = ReplayingLockStore()
    spy.fail("lock_holder", "raise")
    ledger = Ledger(spy, logging.getLogger("t"))
    with pytest.raises(ConnectionError):
        ledger.lock("h", "mine", 60)
    assert spy.inner.lock_holder("h") is None  # not left behind to block the payment


def test_a_definite_refusal_of_other_bytes_on_the_retry_of_a_recorded_payment_is_503(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    spy = SpyStore()
    harness = make(store=spy)
    spy.fail("claim", "raise", "raise", "raise")
    assert harness.pay(1).status_code == 503  # settled, delivery withheld: a settled record
    facilitator.settle_always = refuse("invalid_exact_canton_expired")
    other_bytes = harness.client.get(
        "/paid", headers={"PAYMENT-SIGNATURE": harness.header(tx=("1220" + tx_hash(1)).upper())}
    )
    assert other_bytes.status_code == 503 and other_bytes.json()["error"].startswith(
        "payment outcome unknown"
    )
    assert spy.inner.record(tx_hash(1)) is not None


# -- closing, polling without poll() (runtime lows) -------------------------------------------------------------------------


def test_aclose_does_not_block_the_event_loop_or_outlive_its_timeout_during_a_store_outage(
    facilitator: MockFacilitator, caplog: pytest.LogCaptureFixture
) -> None:
    class SlowUnclaim(SpyStore):
        def unclaim(self, update_id: str, token: str) -> None:
            time.sleep(4.0)
            raise ConnectionError("store down")

    spy = SlowUnclaim()
    server = inproc.build_server(facilitator, store=spy, reconcile_interval=60.0)
    server.ledger._remember(server.ledger._claims, "u", "t", 3600)  # noqa: SLF001  # a release left pending

    async def run() -> tuple[float, int]:
        ticks = 0

        async def ticker() -> None:
            nonlocal ticks
            while True:
                await asyncio.sleep(0.02)
                ticks += 1

        task = asyncio.ensure_future(ticker())
        started = time.monotonic()
        await server.aclose(timeout=0.5)
        task.cancel()
        return time.monotonic() - started, ticks

    elapsed, ticks = asyncio.run(run())
    assert elapsed < 3.0 and ticks > 20  # bounded, and the loop kept running
    assert any("final reconciler pass did not finish" in r.getMessage() for r in caplog.records)


def test_the_socket_check_works_where_there_is_no_poll_and_no_msg_dontwait(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    import ftptech_x402_canton.wsgi as wsgi_module

    monkeypatch.delattr(select, "poll", raising=False)
    monkeypatch.setattr(
        wsgi_module, "_MSG_PEEK_NOWAIT", socket.MSG_PEEK
    )  # Windows defines no MSG_DONTWAIT
    left, right = socket.socketpair()
    try:
        assert socket_gone(left) is False
        right.close()
        assert socket_gone(left) is True
    finally:
        left.close()


def test_a_probe_helper_is_importable() -> None:
    assert Path(__file__).exists() and Seen is not None and Out is not None
    assert inproc is not None and payment_header is not None


@pytest.mark.skipif(not UWSGI.exists(), reason="pyuwsgi needed")
def test_a_chunked_upload_on_real_uwsgi_is_drained_and_a_leaving_payer_is_seen(
    facilitator: MockFacilitator,
) -> None:
    port = free_port()
    env = {**os.environ, "FACILITATOR_URL": facilitator.url, "PYTHONPATH": os.getcwd()}
    process = subprocess.Popen(
        [str(UWSGI), "--http-socket", f"127.0.0.1:{port}", "--module", "tests.gunicorn_app:app", "--lazy-apps",
         "--master", "--processes", "1", "--enable-threads", "--buffer-size", "32768", "--disable-logging"],
        env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )  # fmt: skip
    try:
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            try:
                with socket.create_connection(("127.0.0.1", port), timeout=0.3):
                    break
            except OSError:
                time.sleep(0.2)
        else:
            pytest.skip("uwsgi did not start")
        accepted = offer_post(port)
        header = payment_header(accepted, tx_hash(7))
        chunk = b"c" * 4096
        payload = (
            b"".join(f"{len(chunk):x}\r\n".encode() + chunk + b"\r\n" for _ in range(80))
            + b"0\r\n\r\n"
        )  # 320 KiB
        request = (
            f"POST /post HTTP/1.1\r\nHost: x\r\nPAYMENT-SIGNATURE: {header}\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n"
        ).encode() + payload
        with socket.create_connection(("127.0.0.1", port), timeout=30) as sock:
            sock.sendall(request)
            data = b""
            while b"\r\n\r\n" not in data:
                data += sock.recv(4096)
        # uWSGI hands the app none of a chunked body: the handler's get_json fails (a 4xx/5xx is not settled),
        # but the request must finish, never hang on the drain.
        assert data.startswith(b"HTTP/1.1 ")
    finally:
        process.terminate()
        process.wait(15)
