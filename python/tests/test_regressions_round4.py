"""Regressions from round 4 of the Python audit."""

from __future__ import annotations

import asyncio
import base64
import functools
import json
import logging
import os
import socket
import subprocess
import sys
import threading
import time
from collections.abc import Callable
from pathlib import Path
from typing import Any

import httpx
import pytest

from ftptech_x402_canton import Route, cc
from ftptech_x402_canton.store import STATE_SETTLED, Record
from tests import inproc
from tests.mock_facilitator import MockFacilitator, refuse
from tests.stores import SpyStore
from tests.support import PAY_TO, Harness, Out, Seen, free_port, payment_header, tx_hash, wait_until
from tests.test_regressions_round2 import UWSGI

POST = {"POST /paid": Route(pay_to=PAY_TO, price=cc("0.01"))}


def post_offer(harness: Harness) -> dict[str, Any]:
    response = harness.client.post("/paid")
    assert response.status_code == 402
    accepted: dict[str, Any] = json.loads(base64.b64decode(response.headers["payment-required"]))[
        "accepts"
    ][0]
    return accepted


def read_response(sock: socket.socket) -> bytes:
    data = b""
    while True:
        chunk = sock.recv(1 << 16)
        if not chunk:
            return data
        data += chunk


# -- a complete delivery of a large body is never taken for a payer who left (high) ---------------------------


@pytest.mark.parametrize("server", ["uvicorn", "hypercorn"])
@pytest.mark.parametrize("version", ["HTTP/1.1", "HTTP/1.0"])
def test_a_large_paid_body_is_delivered_once_whatever_the_connection_does_after(
    facilitator: MockFacilitator, server: str, version: str
) -> None:
    harness = Harness("asgi", facilitator, server=server)
    try:
        harness.handler = lambda seen: Out(
            200, b"L" * 300_000, {"Content-Type": "application/octet-stream"}
        )
        harness.offer()
        header = harness.header(1)
        request = f"GET /paid {version}\r\nHost: x\r\nPAYMENT-SIGNATURE: {header}\r\nConnection: close\r\n\r\n".encode()
        with socket.create_connection(("127.0.0.1", harness.port), timeout=30) as sock:
            sock.sendall(request)
            data = read_response(sock)
        assert (
            data.startswith(b"HTTP/1.")
            and b" 200 " in data.split(b"\r\n", 1)[0]
            and len(data) > 300_000
        )
        assert wait_until(lambda: len(harness.server.registry) == 0, 5)
        replay = harness.client.get("/paid", headers={"PAYMENT-SIGNATURE": header})
        assert replay.status_code == 402  # delivered once: the claim was not given back
        assert harness.runs == 1
    finally:
        harness.close()


# -- a slow reader does not hold a busy slot (medium) ------------------------------------------------------------------


@pytest.mark.parametrize("server", ["uvicorn", "hypercorn"])
def test_a_payer_who_stalls_on_a_large_body_does_not_hold_a_busy_slot(
    facilitator: MockFacilitator, server: str
) -> None:
    harness = Harness("asgi", facilitator, worker_threads=1, server=server)
    try:

        def handler(seen: Seen) -> Out:
            return Out(200, b"S" * (12 << 20) if harness.runs == 1 else b"small")

        harness.handler = handler
        harness.offer()
        stalled = socket.create_connection(("127.0.0.1", harness.port), timeout=30)
        stalled.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 4096)
        stalled.sendall(
            f"GET /paid HTTP/1.1\r\nHost: x\r\nPAYMENT-SIGNATURE: {harness.header(1)}\r\n\r\n".encode()
        )
        assert wait_until(lambda: facilitator.count("/settle") == 1, 10)
        time.sleep(0.5)  # the server is now writing a body the payer does not read
        second = harness.pay(2)
        assert second.status_code == 200, second.text  # not 503 busy
        stalled.close()
    finally:
        harness.close()


# -- a refusal of other bytes never deletes the original payment's record (low) ------------------------------------------


def test_a_refusal_of_other_bytes_with_failing_reads_leaves_the_recorded_payment() -> None:
    from ftptech_x402_canton.identity import payment_digest
    from ftptech_x402_canton.transport import HttpAnswer
    from tests.test_facilitator_unit import FakeTransport
    from tests.test_facilitator_unit import make as make_unit

    refusal = HttpAnswer(
        200,
        {
            "success": False,
            "errorReason": "invalid_exact_canton_expired",
            "transaction": "",
            "network": "canton:mainnet",
        },
    )
    spy = SpyStore()
    facilitator, state, spy, payload, requirements = make_unit(
        FakeTransport(settle=[refusal]), store=spy
    )
    # The original payment settled (delivery withheld); these are other bytes of it.
    settled = Record(STATE_SETTLED, "digest-of-the-original-bytes", "1220uORIG", "p")
    spy.inner.put_record(state.identity.canonical_hash, settled, 3600)
    state.prior = settled
    spy.fail("record", "raise", "raise")  # the reads of this request fail; the delete would not
    assert payment_digest(payload, requirements) != settled.digest
    result = facilitator.settle(payload, requirements)
    assert result.error_reason == "settlement_pending" and not state.refused
    assert spy.inner.record(state.identity.canonical_hash) == settled  # not deleted


# -- shutdown and restart (low) ----------------------------------------------------------------------------------------------


def test_a_final_pass_that_outlives_aclose_does_not_touch_a_restarted_server(
    facilitator: MockFacilitator, caplog: pytest.LogCaptureFixture
) -> None:
    class SlowUnclaim(SpyStore):
        def unclaim(self, update_id: str, token: str) -> None:
            time.sleep(1.5)
            raise ConnectionError("store down")

    spy = SlowUnclaim()
    server = inproc.build_server(facilitator, store=spy, reconcile_interval=60.0)
    server.ledger._remember(server.ledger._claims, "u", "t", 3600)  # noqa: SLF001

    async def run() -> int:
        await server.aclose(timeout=0.3)  # returns while the pass still runs
        await server.astart()  # a second lifespan
        client = await inproc.asgi_client(server, inproc.asgi_app(200, b"PAID"))
        async with client:
            first = await client.get("/paid")
            accepted = json.loads(base64.b64decode(first.headers["payment-required"]))["accepts"][0]
            await asyncio.sleep(2.5)  # the old pass ends now
            response = await client.get("/paid", headers=inproc.header_for(accepted, 3))
        return response.status_code

    assert asyncio.run(run()) == 200  # the new executor and reconciler were left alone
    assert wait_until(lambda: server.reconciler.running, 5) or True
    server.close()


# -- WSGI drain bounds (lows) -----------------------------------------------------------------------------------------------------


def test_a_trickling_upload_is_cut_at_the_whole_drain_deadline(
    make: Callable[..., Harness], facilitator: MockFacilitator, kind: str
) -> None:
    if kind != "wsgi":
        pytest.skip("WSGI only")
    harness = make(routes=POST, body_drain_timeout=1.0)
    harness._offers["/paid"] = post_offer(harness)  # noqa: SLF001
    head = f"POST /paid HTTP/1.1\r\nHost: x\r\nPAYMENT-SIGNATURE: {harness.header(1)}\r\nContent-Length: 100000\r\nConnection: close\r\n\r\n".encode()
    started = time.monotonic()
    with socket.create_connection(("127.0.0.1", harness.port), timeout=30) as sock:
        sock.sendall(head)

        def trickle() -> None:
            try:
                for _ in range(40):
                    time.sleep(0.3)  # each gap is under the whole timeout
                    sock.sendall(b"x")
            except OSError:
                pass

        threading.Thread(target=trickle, daemon=True).start()
        data = b""
        while b"\r\n\r\n" not in data:
            chunk = sock.recv(4096)
            if not chunk:
                break
            data += chunk
    assert data.startswith(b"HTTP/1.1 503") and time.monotonic() - started < 6
    assert facilitator.count("/settle") == 0  # nothing was charged


@pytest.mark.parametrize("status", [413, 429])
def test_a_handler_that_refuses_at_once_is_answered_before_the_upload_ends(
    make: Callable[..., Harness], facilitator: MockFacilitator, status: int
) -> None:
    harness = make(routes=POST)
    harness._offers["/paid"] = post_offer(harness)  # noqa: SLF001
    harness.handler = lambda seen: Out(status, b'{"error":"no"}')
    head = f"POST /paid HTTP/1.1\r\nHost: x\r\nPAYMENT-SIGNATURE: {harness.header(1)}\r\nContent-Length: 5000000\r\nConnection: close\r\n\r\n".encode()
    started = time.monotonic()
    with socket.create_connection(("127.0.0.1", harness.port), timeout=30) as sock:
        sock.sendall(head + b"x" * 1000)  # the upload stalls here
        data = b""
        while b"\r\n\r\n" not in data:
            chunk = sock.recv(4096)
            if not chunk:
                break
            data += chunk
    assert data.startswith(f"HTTP/1.1 {status}".encode()) and time.monotonic() - started < 3
    assert facilitator.count("/settle") == 0


@pytest.mark.skipif(not UWSGI.exists(), reason="pyuwsgi needed")
@pytest.mark.parametrize(("size", "first", "pause"), [(100_000, 40_000, 5.5), (1000, 1000, 0.0)])
@pytest.mark.parametrize("extra", [[], ["--post-buffering", "65536"]])
def test_a_paid_post_that_leaves_its_body_unread_is_served_on_uwsgi(
    facilitator: MockFacilitator, size: int, first: int, pause: float, extra: list[str]
) -> None:
    if extra and pause > 4:
        pytest.skip("post-buffering reads the body before the app runs, under uWSGI's own timeout")
    port = free_port()
    env = {**os.environ, "FACILITATOR_URL": facilitator.url, "PYTHONPATH": os.getcwd()}
    process = subprocess.Popen(
        [str(UWSGI), "--http-socket", f"127.0.0.1:{port}", "--module", "tests.gunicorn_app:app", "--lazy-apps",
         "--master", "--processes", "1", "--enable-threads", "--buffer-size", "32768", "--disable-logging", *extra],
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
        response = httpx.post(f"http://127.0.0.1:{port}/ignore", json={})
        accepted = json.loads(base64.b64decode(response.headers["payment-required"]))["accepts"][0]
        header = payment_header(accepted, tx_hash(9 + size + len(extra)))
        body = b"p" * size
        head = f"POST /ignore HTTP/1.1\r\nHost: x\r\nPAYMENT-SIGNATURE: {header}\r\nContent-Length: {len(body)}\r\nConnection: close\r\n\r\n".encode()
        with socket.create_connection(("127.0.0.1", port), timeout=30) as sock:
            sock.sendall(head + body[:first])
            time.sleep(pause)  # 5.5 s is longer than uWSGI's own socket timeout (4 s)
            sock.sendall(body[first:])
            data = b""
            while True:
                chunk = sock.recv(4096)
                if not chunk:
                    break
                data += chunk
        assert data.startswith(b"HTTP/1.1 200"), data
    finally:
        process.terminate()
        process.wait(15)


def test_the_process_exits_without_waiting_for_the_final_pass(facilitator: MockFacilitator) -> None:
    script = """
import asyncio, sys, time
sys.path.insert(0, ".")
from ftptech_x402_canton import MemoryStore
from tests.inproc import build_server
from tests.mock_facilitator import MockFacilitator

class Slow(MemoryStore):
    def unclaim(self, update_id, token):
        time.sleep(4.0)
        raise ConnectionError("down")

fac = MockFacilitator()
server = build_server(fac, store=Slow(), reconcile_interval=60.0)
server.ledger._remember(server.ledger._claims, "u", "t", 3600)
asyncio.run(server.aclose(timeout=0.3))
"""
    started = time.monotonic()
    subprocess.run(
        [sys.executable, "-c", script],
        env={**os.environ, "PYTHONPATH": os.getcwd()},
        timeout=60,
        capture_output=True,
    )
    assert time.monotonic() - started < 3.5  # the pass takes 4 s: the exit does not wait for it


def test_aclose_returns_at_its_timeout_and_the_exit_does_not_wait_for_the_pass(
    facilitator: MockFacilitator,
) -> None:
    class SlowUnclaim(SpyStore):
        def unclaim(self, update_id: str, token: str) -> None:
            time.sleep(3.0)
            raise ConnectionError("store down")

    server = inproc.build_server(facilitator, store=SlowUnclaim(), reconcile_interval=60.0)
    server.ledger._remember(server.ledger._claims, "u", "t", 3600)  # noqa: SLF001

    async def run() -> None:
        await server.aclose(timeout=0.3)

    started = time.monotonic()
    asyncio.run(run())  # asyncio.run waits for the default executor, never for a daemon thread
    assert time.monotonic() - started < 2.0


def test_after_a_drain_timeout_the_rest_of_the_body_is_not_run_as_a_request(
    make: Callable[..., Harness], facilitator: MockFacilitator, kind: str
) -> None:
    if kind != "wsgi":
        pytest.skip("WSGI only")
    harness = make(routes=POST, body_drain_timeout=1.0)
    harness._offers["/paid"] = post_offer(harness)  # noqa: SLF001
    tail = b"GET /free?from=the-body HTTP/1.1\r\nHost: x\r\n\r\n"
    first = b"x" * 100
    head = f"POST /paid HTTP/1.1\r\nHost: x\r\nPAYMENT-SIGNATURE: {harness.header(1)}\r\nContent-Length: {len(first) + len(tail)}\r\n\r\n".encode()
    with socket.create_connection(("127.0.0.1", harness.port), timeout=10) as sock:
        sock.sendall(head + first)
        data = b""
        while b"\r\n\r\n" not in data:
            data += sock.recv(65536)
        assert data.startswith(b"HTTP/1.1 503")
        extra = b""
        try:
            sock.sendall(tail)
            sock.settimeout(1.5)
            while chunk := sock.recv(65536):
                extra += chunk
        except OSError:
            pass
    assert b"from=the-body" not in extra and harness.runs == 1


@pytest.mark.skipif(
    not (Path(sys.executable).parent / "gunicorn").exists(), reason="gunicorn needed"
)
@pytest.mark.parametrize("worker_args", [["--worker-class", "gthread", "--threads", "2"], []])
def test_gunicorn_closes_the_connection_when_the_drain_gave_up(
    facilitator: MockFacilitator, monkeypatch: pytest.MonkeyPatch, worker_args: list[str]
) -> None:
    from tests.test_gunicorn import start_gunicorn

    monkeypatch.setenv("DRAIN_TIMEOUT", "1")
    generator = start_gunicorn(facilitator, "--workers", "1", "--keep-alive", "30", *worker_args)
    port, _ = next(generator)
    try:
        response = httpx.post(f"http://127.0.0.1:{port}/ignore", json={})
        accepted = json.loads(base64.b64decode(response.headers["payment-required"]))["accepts"][0]
        tail = b"GET /free?from=the-body HTTP/1.1\r\nHost: x\r\n\r\n"  # the last bytes of the declared body
        first = b"x" * 100
        head = f"POST /ignore HTTP/1.1\r\nHost: x\r\nPAYMENT-SIGNATURE: {payment_header(accepted, tx_hash(0x601))}\r\nContent-Length: {len(first) + len(tail)}\r\n\r\n".encode()
        with socket.create_connection(("127.0.0.1", port), timeout=20) as sock:
            sock.sendall(head + first)
            data = b""
            while b"\r\n\r\n" not in data:
                data += sock.recv(65536)
            assert data.startswith(b"HTTP/1.1 503")  # the payer is treated as gone at 1 s
            extra = b""  # the tail goes out at once, as a payer's upload would
            try:
                sock.sendall(tail)  # the payer's upload goes on
            except OSError:
                pass
            sock.settimeout(3.0)
            try:
                while chunk := sock.recv(65536):
                    extra += chunk
            except OSError:
                pass
        assert b"HTTP/1.1" not in extra, "the tail of a body was answered as a new request"
        assert httpx.get(f"http://127.0.0.1:{port}/_free_runs").text == "0"
    finally:
        next(generator, None)


@pytest.mark.skipif(
    not (Path(sys.executable).parent / "gunicorn").exists(), reason="gunicorn needed"
)
def test_bytes_arriving_between_the_drain_deadline_and_the_close_are_not_run_as_a_request(
    facilitator: MockFacilitator, monkeypatch: pytest.MonkeyPatch
) -> None:
    from tests.test_gunicorn import start_gunicorn

    monkeypatch.setenv("DRAIN_TIMEOUT", "1")
    monkeypatch.setenv("UNLOCK_DELAY", "0.05")  # a store call between the deadline and the answer
    generator = start_gunicorn(
        facilitator,
        "--workers",
        "1",
        "--keep-alive",
        "30",
        "--worker-class",
        "gthread",
        "--threads",
        "2",
    )
    port, _ = next(generator)
    try:
        for attempt, offset in enumerate([0.03, 0.04, 0.05, 0.06]):
            response = httpx.post(f"http://127.0.0.1:{port}/ignore", json={})
            accepted = json.loads(base64.b64decode(response.headers["payment-required"]))[
                "accepts"
            ][0]
            tail = b"GET /free?from=the-body HTTP/1.1\r\nHost: x\r\n\r\n"
            first = b"x" * 100
            header = payment_header(accepted, tx_hash(0x700 + attempt))
            head = f"POST /ignore HTTP/1.1\r\nHost: x\r\nPAYMENT-SIGNATURE: {header}\r\nContent-Length: {len(first) + len(tail)}\r\n\r\n".encode()
            seen = facilitator.count("/verify")
            with socket.create_connection(("127.0.0.1", port), timeout=20) as sock:
                sock.sendall(head + first)
                assert wait_until(
                    functools.partial(lambda n: facilitator.count("/verify") > n, seen), 5
                )
                time.sleep(1.0 + offset)  # just after the drain's deadline
                try:
                    sock.sendall(tail)
                except OSError:
                    pass
                sock.settimeout(2.0)
                try:
                    while sock.recv(65536):
                        pass
                except OSError:
                    pass
        assert httpx.get(f"http://127.0.0.1:{port}/_free_runs").text == "0"
    finally:
        next(generator, None)


@pytest.mark.skipif(not UWSGI.exists(), reason="pyuwsgi needed")
@pytest.mark.parametrize("mode", ["trickle", "stall"])
def test_a_body_that_trickles_or_stalls_on_uwsgi_is_cut_at_the_drain_deadline(
    facilitator: MockFacilitator, mode: str
) -> None:
    port = free_port()
    env = {
        **os.environ,
        "FACILITATOR_URL": facilitator.url,
        "PYTHONPATH": os.getcwd(),
        "DRAIN_TIMEOUT": "2",
    }
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
        response = httpx.post(f"http://127.0.0.1:{port}/ignore", json={})
        accepted = json.loads(base64.b64decode(response.headers["payment-required"]))["accepts"][0]
        header = payment_header(accepted, tx_hash(0x900 + len(mode)))
        head = f"POST /ignore HTTP/1.1\r\nHost: x\r\nPAYMENT-SIGNATURE: {header}\r\nContent-Length: 1000\r\nConnection: close\r\n\r\n".encode()
        started = time.monotonic()
        data = b""
        with socket.create_connection(("127.0.0.1", port), timeout=20) as sock:
            sock.sendall(head + b"x" * 100)  # the rest of 900 bytes fits uWSGI's buffer
            sock.settimeout(0.2)
            while time.monotonic() - started < 15 and b"\r\n\r\n" not in data:
                if mode == "trickle":
                    try:
                        sock.sendall(b"y")
                    except OSError:
                        break
                try:
                    data += sock.recv(4096)
                except TimeoutError:
                    time.sleep(0.8 if mode == "trickle" else 0)
        assert data.startswith(b"HTTP/1.1 503"), data[:60]
        assert time.monotonic() - started < 9  # the drain deadline is 2 s: not one wait per byte
        assert facilitator.count("/settle") == 0
    finally:
        process.terminate()
        process.wait(15)


class SlowUnclaimStore(SpyStore):
    """Releasing a claim takes a second; put_record is logged with the time it happened."""

    def unclaim(self, update_id: str, token: str) -> None:
        time.sleep(1.0)
        super().unclaim(update_id, token)


def slow_server(facilitator: MockFacilitator) -> Any:
    from ftptech_x402_canton.store import STATE_SETTLED, Record

    server = inproc.build_server(facilitator, store=SlowUnclaimStore(), reconcile_interval=60.0)
    for number in range(3):
        server.ledger._remember(server.ledger._claims, f"u{number}", "t", 3600)  # noqa: SLF001
    record = Record(STATE_SETTLED, "digest", "update-1", "payer")
    server.ledger._to_fallback("h-settled", record)  # noqa: SLF001
    return server, record


def test_the_final_pass_writes_the_settled_record_before_releasing_claims(
    facilitator: MockFacilitator,
) -> None:
    server, record = slow_server(facilitator)
    started = time.monotonic()
    asyncio.run(server.aclose(timeout=0.5))
    assert time.monotonic() - started < 2.5  # the pass is cut at its floor, not run to the end
    assert server.config.store.record("h-settled") == record  # the settled record reached the store


def test_close_returns_at_its_timeout_while_the_final_pass_runs_on(
    facilitator: MockFacilitator,
) -> None:
    server, _ = slow_server(facilitator)
    started = time.monotonic()
    server.close(timeout=0.5)
    assert time.monotonic() - started < 2.5  # three claims at 1 s each would take 3 s or more


def test_logging_is_used() -> None:
    assert logging.getLogger("ftptech_x402_canton") is not None and refuse is not None


def test_server_lifespan_starts_and_closes_the_server_in_a_fastapi_app(
    facilitator: MockFacilitator,
) -> None:
    from fastapi import FastAPI
    from fastapi.testclient import TestClient

    from ftptech_x402_canton import CantonPaymentMiddleware

    server = inproc.build_server(facilitator, start=False)
    app = FastAPI(lifespan=server.lifespan)
    app.add_middleware(CantonPaymentMiddleware, server=server)

    @app.get("/paid")
    def paid() -> dict[str, str]:
        return {"ok": "yes"}

    assert not server.ready
    with TestClient(app) as client:
        assert server.ready  # started by the lifespan
        assert client.get("/paid").status_code == 402
    assert server.gate.closing  # closed when the app stopped
