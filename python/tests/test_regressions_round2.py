"""Regressions from round 2 of the Python audit (money, runtime and fresh-eyes lenses)."""

from __future__ import annotations

import asyncio
import logging
import os
import shutil
import socket
import subprocess
import sys
import threading
import time
import types
from collections.abc import Callable, MutableMapping
from pathlib import Path
from typing import Any

import httpx
import pytest

from ftptech_x402_canton import CantonPaymentMiddleware, CantonWSGIMiddleware, RedisStore
from ftptech_x402_canton.ledger import Ledger
from ftptech_x402_canton.paywall import display_asset
from ftptech_x402_canton.scheme import FtpCantonScheme
from ftptech_x402_canton.store import STATE_ATTEMPT, Record
from ftptech_x402_canton.transport import Transport
from ftptech_x402_canton.wsgi import _uwsgi_gone, socket_gone
from tests import inproc
from tests.mock_facilitator import PAYER, Answer, MockFacilitator, refuse
from tests.stores import FakeRedis, SpyStore
from tests.support import PAY_TO, Harness, Out, Seen, free_port, payment_header, tx_hash, wait_until

POST = {
    "POST /paid": __import__("ftptech_x402_canton").Route(
        pay_to=PAY_TO, price=__import__("ftptech_x402_canton").cc("0.01")
    )
}


def post_offer(harness: Harness) -> dict[str, Any]:
    import base64
    import json

    response = harness.client.post("/paid")
    assert response.status_code == 402
    accepted: dict[str, Any] = json.loads(base64.b64decode(response.headers["payment-required"]))[
        "accepts"
    ][0]
    return accepted


# -- hypercorn: nothing blocks the gate's own answer once the unread cap is passed (runtime high) ------------


@pytest.mark.parametrize("server", ["hypercorn", "uvicorn"])
def test_a_body_past_the_unread_cap_is_answered_and_frees_its_slot(
    facilitator: MockFacilitator, server: str
) -> None:
    harness = Harness(
        "asgi",
        facilitator,
        routes=POST,
        server=server,
        max_unread_body_bytes=1 << 20,
        worker_threads=1,
    )
    try:
        harness._offers["/paid"] = post_offer(harness)  # noqa: SLF001

        def slow(seen: Seen) -> Out:
            time.sleep(0.5)
            return Out()

        harness.handler = slow
        started = time.monotonic()
        response = harness.client.post(
            "/paid",
            content=b"u" * (6 << 20),
            headers={"PAYMENT-SIGNATURE": harness.header(1)},
            timeout=30,
        )
        assert time.monotonic() - started < 20
        assert response.status_code == 503  # the payer is treated as gone: nothing was settled
        assert facilitator.count("/settle") == 0
        harness.handler = lambda seen: Out()
        # The slot was freed: with worker_threads=1 the next paid request is not "busy".
        again = harness.client.post(
            "/paid", content=b"x", headers={"PAYMENT-SIGNATURE": harness.header(2)}, timeout=20
        )
        assert again.status_code == 200, again.text
    finally:
        harness.close()


# -- uWSGI: a FIN is seen through a peek on a duplicate descriptor (runtime high) --------------------------------


def test_the_uwsgi_check_sees_a_fin_that_is_connected_does_not(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    left, right = socket.socketpair()
    fake = types.ModuleType("uwsgi")
    vars(fake).update(
        connection_fd=lambda: left.fileno(),
        is_connected=lambda fd: True,  # reads only the socket error state: a FIN is not an error
    )
    monkeypatch.setitem(sys.modules, "uwsgi", fake)
    try:
        assert _uwsgi_gone() is False
        right.shutdown(socket.SHUT_WR)  # the payer closes: a FIN
        assert _uwsgi_gone() is True
        assert left.fileno() != -1  # the duplicate was closed, the original left alone
    finally:
        left.close()
        right.close()


UWSGI = Path(sys.executable).parent / "uwsgi"


@pytest.mark.skipif(not UWSGI.exists() or shutil.which("sh") is None, reason="pyuwsgi needed")
def test_a_payer_who_leaves_during_settle_on_real_uwsgi_is_served_on_retry(
    facilitator: MockFacilitator,
) -> None:
    port = free_port()
    env = {**os.environ, "FACILITATOR_URL": facilitator.url, "PYTHONPATH": os.getcwd()}
    process = subprocess.Popen(
        [str(UWSGI), "--http-socket", f"127.0.0.1:{port}", "--module", "tests.gunicorn_app:app", "--lazy-apps",
         "--master", "--processes", "1", "--enable-threads", "--buffer-size", "32768", "--disable-logging"],
        env=env,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
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
        import base64
        import json

        first = httpx.get(f"http://127.0.0.1:{port}/paid", timeout=20)
        accepted = json.loads(base64.b64decode(first.headers["payment-required"]))["accepts"][0]
        facilitator.settle_queue.append(
            Answer(
                200,
                {
                    "success": True,
                    "transaction": "1220uUWSGI",
                    "network": "canton:mainnet",
                    "payer": PAYER,
                },
                delay=1.0,
            )
        )
        header = payment_header(accepted, tx_hash(5))
        sock = socket.create_connection(("127.0.0.1", port), timeout=20)
        sock.sendall(
            f"GET /paid HTTP/1.1\r\nHost: x\r\nPAYMENT-SIGNATURE: {header}\r\nConnection: close\r\n\r\n".encode()
        )
        assert wait_until(lambda: facilitator.count("/settle") == 1, 15)
        sock.shutdown(socket.SHUT_RDWR)
        sock.close()
        time.sleep(1.5)
        retry = httpx.get(
            f"http://127.0.0.1:{port}/paid", headers={"PAYMENT-SIGNATURE": header}, timeout=30
        )
        assert retry.status_code == 200, retry.text
        assert facilitator.count("/settle") == 1
    finally:
        process.terminate()
        process.wait(15)


# -- waitress defaults (runtime medium) -------------------------------------------------------------------------


def test_waitress_with_default_lookahead_is_not_trusted_and_says_so_loudly(
    facilitator: MockFacilitator, caplog: pytest.LogCaptureFixture, monkeypatch: pytest.MonkeyPatch
) -> None:
    import ftptech_x402_canton.wsgi as wsgi_module

    monkeypatch.setattr(wsgi_module, "_warned", set())
    harness = Harness("wsgi", facilitator, server="waitress-default")
    try:
        assert harness.pay(1).status_code == 200
        assert harness.pay(2).status_code == 200
        errors = [r for r in caplog.records if "channel_request_lookahead=0" in r.getMessage()]
        assert len(errors) == 1 and errors[0].levelno == logging.ERROR
        assert "payment_already_redeemed" in errors[0].getMessage()
    finally:
        harness.close()


# -- a FIN behind unread body bytes, and the WSGI drain bound (runtime medium) -----------------------------------


def test_a_fin_is_seen_even_behind_unread_bytes() -> None:
    left, right = socket.socketpair()
    try:
        right.sendall(b"x" * 100_000)  # unread request body
        assert socket_gone(left) is False
        right.shutdown(
            socket.SHUT_WR
        )  # a bare FIN behind the unread bytes: a peek would see only data
        assert socket_gone(left) is True
    finally:
        left.close()


def test_a_wsgi_body_past_the_bound_is_treated_as_a_payer_who_left(
    make: Callable[..., Harness], facilitator: MockFacilitator, kind: str
) -> None:
    if kind != "wsgi":
        pytest.skip("WSGI only")
    harness = make(routes=POST, max_unread_body_bytes=2000)
    harness._offers["/paid"] = post_offer(harness)  # noqa: SLF001
    response = harness.client.post(
        "/paid",
        content=b"b" * 100_000,
        headers={"PAYMENT-SIGNATURE": harness.header(1)},
        timeout=20,
    )
    assert response.status_code == 503 and facilitator.count("/settle") == 0  # nothing was charged


# -- a failed clear never touches a newer attempt (money medium) --------------------------------------------------


def test_a_definite_refusal_whose_delete_fails_leaves_the_attempt_and_the_retry_is_delivered(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    spy = SpyStore()
    harness = make(store=spy)
    facilitator.settle_queue.append(refuse("invalid_exact_canton_expired"))
    spy.fail("delete_record", "raise")
    assert harness.pay(1).status_code == 402
    assert spy.inner.record(tx_hash(1)) is not None and not harness.server.ledger.has_pending()
    retry = harness.pay(1)  # the same bytes: the stale attempt only skips /verify, /settle decides
    assert retry.status_code == 200
    assert harness.pay(1).status_code == 402  # delivered once


def test_an_old_attempt_stays_unknown_when_the_store_blips_on_the_retry() -> None:
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
    from ftptech_x402_canton.identity import payment_digest

    old = Record(
        STATE_ATTEMPT, payment_digest(payload, requirements), at=int(time.time()) - 30 * 3600
    )
    state.prior = old  # what the guard read
    spy.fail("record", "raise", "raise")  # the lookups fail
    result = facilitator.settle(payload, requirements)
    assert (
        result.error_reason == "settlement_pending" and not state.refused
    )  # not cleared, not a 402


# -- busy, burst, unpaid offers (money medium, runtime lows) -------------------------------------------------------


def test_a_burst_of_paid_requests_cannot_pass_the_busy_cap(
    make: Callable[..., Harness], facilitator: MockFacilitator, kind: str
) -> None:
    if kind != "asgi":
        pytest.skip("ASGI only")
    harness = make(worker_threads=2)
    harness.offer()
    facilitator.settle_always = Answer(
        200, {"success": True, "transaction": "1220uB", "network": "canton:mainnet"}, delay=1.0
    )
    facilitator.settle_always = None
    for n in range(1, 9):
        facilitator.settle_queue.append(
            Answer(
                200,
                {
                    "success": True,
                    "transaction": facilitator.update_id(tx_hash(n)),
                    "network": "canton:mainnet",
                    "payer": PAYER,
                },
                delay=1.0,
            )
        )
    barrier = threading.Barrier(8)
    codes: list[int] = []

    def go(n: int) -> None:
        barrier.wait()
        codes.append(harness.pay(n).status_code)

    threads = [threading.Thread(target=go, args=(n,)) for n in range(1, 9)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(30)
    assert codes.count(503) >= 5, codes  # at most worker_threads (2) were admitted
    assert facilitator.count("/verify") <= 3


def test_the_unpaid_offer_needs_no_worker_thread(
    make: Callable[..., Harness], kind: str, monkeypatch: pytest.MonkeyPatch
) -> None:
    if kind != "asgi":
        pytest.skip("ASGI only")
    harness = make(
        store=RedisStore(FakeRedis(), "p:")
    )  # a blocking store: begin used to hop to the pool
    monkeypatch.setattr(
        type(harness.server),
        "executor",
        property(lambda self: (_ for _ in ()).throw(RuntimeError("pool is full"))),
    )
    response = harness.client.get("/paid")
    assert response.status_code == 402 and "payment-required" in response.headers


def test_nothing_is_written_to_a_connection_the_server_gave_up_on() -> None:
    from ftptech_x402_canton.asgi import ReceivePump, write_response
    from ftptech_x402_canton.responses import gate_error

    async def run() -> int:
        sent: list[Any] = []

        async def receive() -> dict[str, Any]:
            await asyncio.sleep(3600)
            return {"type": "http.disconnect"}

        async def send(message: MutableMapping[str, Any]) -> None:
            sent.append(message)

        pump = ReceivePump(receive)
        pump.mark_gone()
        await write_response(send, gate_error(503, "x"), pump=pump)
        return len(sent)

    assert asyncio.run(run()) == 0


def test_an_app_that_returns_before_its_last_chunk_is_not_settled(
    facilitator: MockFacilitator,
) -> None:
    server = inproc.build_server(facilitator)

    async def cut(scope: Any, receive: Any, send: Any) -> None:
        await send({"type": "http.response.start", "status": 200, "headers": []})
        await send(
            {"type": "http.response.body", "body": b"fragment", "more_body": True}
        )  # and returns

    async def run() -> int:
        client = await inproc.asgi_client(server, inproc.asgi_app(200))
        async with client:
            first = await client.get("/paid")
        import base64
        import json

        accepted = json.loads(base64.b64decode(first.headers["payment-required"]))["accepts"][0]
        sent: list[MutableMapping[str, Any]] = []

        receive = inproc.idle_receive()

        async def send(message: MutableMapping[str, Any]) -> None:
            sent.append(message)

        scope = {
            "type": "http", "method": "GET", "path": "/paid", "raw_path": b"/paid", "query_string": b"",
            "headers": [(b"payment-signature", payment_header(accepted, tx_hash(1)).encode()), (b"host", b"x")],
            "scheme": "http",
        }  # fmt: skip
        await CantonPaymentMiddleware(cut, server)(scope, receive, send)
        return int(sent[0]["status"])

    assert asyncio.run(run()) == 502
    assert facilitator.count("/settle") == 0
    server.close()


# -- closing (Go lessons) ---------------------------------------------------------------------------------------------


def test_once_close_has_begun_a_payment_is_refused_and_each_caller_waits_for_itself(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make()

    def slow(seen: Seen) -> Out:
        time.sleep(1.0)
        return Out()

    harness.handler = slow
    harness.offer()
    header = harness.header(1)
    first: list[int] = []
    worker = threading.Thread(
        target=lambda: first.append(
            harness.client.get(
                "/paid", headers={"PAYMENT-SIGNATURE": header}, timeout=20
            ).status_code
        ),
        daemon=True,
    )
    worker.start()
    assert wait_until(lambda: len(harness.server.registry) == 1, 5)
    closers = [threading.Thread(target=lambda: harness.server.close(timeout=10), daemon=True)]
    closers[0].start()
    assert wait_until(lambda: harness.server.gate.closing, 5)
    refused = harness.client.get("/paid", headers={"PAYMENT-SIGNATURE": harness.header(2)})
    assert refused.status_code == 503 and refused.json() == {
        "error": "payment in progress: retry the same payment"
    }
    assert harness.client.get("/paid").status_code == 402  # the unpaid offer is still answered
    worker.join(20)
    closers[0].join(20)
    assert first == [200] and facilitator.count("/settle") == 1


def test_a_second_close_caller_waits_only_for_its_own_timeout(facilitator: MockFacilitator) -> None:
    from ftptech_x402_canton.identity import PaymentIdentity
    from ftptech_x402_canton.state import RequestState

    server = inproc.build_server(facilitator)
    server.registry.add(
        RequestState(PaymentIdentity("a" * 64, "a" * 64), "t", 1.0, lambda: False)
    )  # a request in flight
    slow = threading.Thread(target=lambda: server.close(timeout=5), daemon=True)
    slow.start()
    time.sleep(0.2)
    started = time.monotonic()
    server.close(timeout=0.2)  # the second caller: its own, shorter timeout
    assert time.monotonic() - started < 1.0
    slow.join(10)


def test_a_closed_server_starts_again(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make()
    assert harness.pay(1).status_code == 200
    harness.server.close()
    harness.server.start()  # a second lifespan: a test client per test, Mangum per invocation
    assert harness.server.ready and not harness.server.gate.closing
    assert harness.pay(2).status_code == 200
    assert wait_until(lambda: harness.server.reconciler.running, 5)


def test_aclose_then_astart_works() -> None:
    mock = MockFacilitator().start()
    server = inproc.build_server(mock)

    async def run() -> bool:
        await server.aclose()
        await server.astart()
        return server.ready

    try:
        assert asyncio.run(run())
    finally:
        server.close()
        mock.stop()


def test_close_defaults_to_the_request_budget(
    monkeypatch: pytest.MonkeyPatch, facilitator: MockFacilitator
) -> None:
    import ftptech_x402_canton.server as server_module

    server = inproc.build_server(facilitator)
    monkeypatch.setattr(server_module, "request_budget", lambda config: 0.2)
    from ftptech_x402_canton.state import RequestState

    ident = __import__("ftptech_x402_canton.identity", fromlist=["x"]).PaymentIdentity(
        "a" * 64, "a" * 64
    )
    server.registry.add(RequestState(ident, "t", 1.0, lambda: False))
    started = time.monotonic()
    server.close()
    assert 0.15 < time.monotonic() - started < 3


# -- lock re-send, redis, workers, packaging (fresh lens) ------------------------------------------------------------------


class ReplayingLockStore(SpyStore):
    """A store whose SET NX is re-sent by the client library after a lost reply: applied, then "taken"."""

    def lock(self, hash: str, token: str, ttl: float) -> bool:
        self.calls.append(("lock", (hash, token, ttl)))
        self.inner.lock(hash, token, ttl)
        return False


def test_a_lock_whose_resent_set_nx_says_taken_is_recognised_as_our_own() -> None:
    spy = ReplayingLockStore()
    ledger = Ledger(spy, logging.getLogger("t"))
    assert ledger.lock("h", "mine", 60) is True
    spy.inner.unlock("h", "mine")
    spy.inner.lock("h", "theirs", 60)
    assert ledger.lock("h", "mine", 60) is False  # someone else's lock stays someone else's


def test_the_memory_store_with_several_workers_is_an_error_in_the_log(
    make: Callable[..., Harness], caplog: pytest.LogCaptureFixture, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("WEB_CONCURRENCY", "4")
    harness = make()
    harness.pay(1)
    harness.pay(2)
    errors = [
        r for r in caplog.records if "memory store with more than one worker" in r.getMessage()
    ]
    assert len(errors) == 1 and errors[0].levelno == logging.ERROR


def test_a_forked_memory_store_is_an_error_in_the_log(
    make: Callable[..., Harness], caplog: pytest.LogCaptureFixture
) -> None:
    harness = make()
    harness.server._creator_pid = os.getpid() + 1  # noqa: SLF001  # "built in another process": a preloaded fork
    harness.pay(1)
    assert any("memory store with more than one worker" in r.getMessage() for r in caplog.records)


def test_the_package_needs_no_framework_extra_and_pins_the_tested_sdk() -> None:
    text = (Path(__file__).parent.parent / "pyproject.toml").read_text()
    assert (
        "x402[" not in text
    )  # no extra forwards to the SDK's own (fastapi 0.162+, starlette 1.3+, flask 3.1.3+)
    assert '"x402>=2.28,<2.29"' in text


def test_the_scheme_offers_authorization_only_and_the_mirror_is_unchanged() -> None:
    assert tuple(FtpCantonScheme().payment_flows["transfer-factory"]["supported"]) == (
        "authorization",
    )
    from ftptech_x402_canton.mechanisms.canton.exact import ExactCantonScheme

    assert tuple(ExactCantonScheme().payment_flows["transfer-factory"]["supported"]) == (
        "authorization",
        "upfront",
    )


def test_the_browser_page_names_a_token_by_its_id() -> None:
    assert display_asset("usdcx-admin::1220" + "dd" * 32 + "::USDCx") == "USDCx"
    assert display_asset("CC") == "CC"


def test_settle_backoff_is_capped_so_the_budget_counts_it() -> None:
    from ftptech_x402_canton.facilitator import CantonFacilitator, FacilitatorSettings
    from ftptech_x402_canton.state import RequestState
    from tests.test_facilitator_unit import FakeTransport

    delays: list[float] = []
    ledger = Ledger(SpyStore(), logging.getLogger("t"))
    facilitator = CantonFacilitator(
        FakeTransport(),  # type: ignore[arg-type]
        ledger,
        FacilitatorSettings(backoff=(10.0, 20.0, 40.0), retry_after_cap=5.0, sleep=delays.append),
        logging.getLogger("t"),
    )
    ident = __import__("ftptech_x402_canton.identity", fromlist=["x"]).PaymentIdentity(
        "a" * 64, "a" * 64
    )
    state = RequestState(ident, "t", 1.0, lambda: False)
    assert [facilitator._delay(state, n) for n in range(3)] == [5.0, 5.0, 5.0]  # noqa: SLF001


def test_the_transport_never_follows_a_redirect(facilitator: MockFacilitator) -> None:
    facilitator.settle_always = Answer(302, {"x": 1}, {"Location": facilitator.url + "/supported"})
    answer = Transport(facilitator.url).post(
        "/settle", b'{"paymentPayload":{"payload":{"preparedTxHash":"' + b"a" * 64 + b'"}}}', 5.0
    )
    assert answer.status == 302


def test_a_paid_route_before_start_logs_once_why(
    facilitator: MockFacilitator, caplog: pytest.LogCaptureFixture
) -> None:
    server = inproc.build_server(facilitator, start=False)
    with inproc.wsgi_client(server, inproc.wsgi_app("200 OK")) as client:
        assert client.get("/paid").status_code == 503
        assert client.get("/paid").status_code == 503
    assert (
        len([r for r in caplog.records if "before CantonServer.start() finished" in r.getMessage()])
        == 1
    )
    server.close()


def test_mangum_is_unsupported_and_says_so_once(
    facilitator: MockFacilitator, caplog: pytest.LogCaptureFixture
) -> None:
    import ftptech_x402_canton.asgi as asgi_module

    asgi_module._lambda_warned = False  # noqa: SLF001
    server = inproc.build_server(facilitator)
    sent: list[Any] = []

    async def receive() -> dict[str, Any]:
        return {"type": "http.request", "body": b"", "more_body": False}

    async def send(message: MutableMapping[str, Any]) -> None:
        sent.append(message)

    scope = {
        "type": "http",
        "method": "GET",
        "path": "/paid",
        "query_string": b"",
        "headers": [(b"payment-signature", b"junk")],
        "aws.event": {},
    }
    asyncio.run(CantonPaymentMiddleware(inproc.asgi_app(200), server)(scope, receive, send))
    assert any("Lambda (Mangum) is unsupported" in r.getMessage() for r in caplog.records)
    server.close()


def test_start_response_without_write_gets_the_body_as_the_return_value(
    facilitator: MockFacilitator,
) -> None:
    """Starlette's WSGIMiddleware, asgiref's WsgiToAsgi and uvicorn --interface wsgi return None from start_response."""
    server = inproc.build_server(facilitator)
    status: list[str] = []

    def start_response(s: str, h: list[tuple[str, str]], e: Any = None) -> None:
        status.append(s)

    environ = {
        "REQUEST_METHOD": "GET", "PATH_INFO": "/paid", "SCRIPT_NAME": "", "QUERY_STRING": "",
        "SERVER_NAME": "x", "wsgi.url_scheme": "http", "HTTP_HOST": "x",
    }  # fmt: skip
    body = b"".join(
        CantonWSGIMiddleware(inproc.wsgi_app("200 OK"), server)(environ, start_response)
    )
    assert status[0].startswith("402") and b"{" in body  # the offer reaches the client
    server.close()
