"""Lessons of the two Go audit rounds, as they apply to Python."""

from __future__ import annotations

import asyncio
import base64
import io
import json
from collections.abc import Callable, MutableMapping
from typing import Any

from ftptech_x402_canton import (
    CantonPaymentMiddleware,
    CantonWSGIMiddleware,
    MemoryStore,
    Route,
    cc,
)
from ftptech_x402_canton.asgi import ReceivePump
from ftptech_x402_canton.framing import DRAIN_BOUND
from ftptech_x402_canton.wsgi import drain_input
from tests import inproc
from tests.mock_facilitator import Answer, MockFacilitator
from tests.stores import SpyStore
from tests.support import PAY_TO, Harness, payment_header, tx_hash, wait_until

# -- an unread request body must not hide a disconnect --------------------------------


def test_an_unread_request_body_does_not_hide_a_disconnect(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make(routes={"POST /paid": Route(pay_to=PAY_TO, price=cc("0.01"))})
    harness.offer_method = "POST"  # type: ignore[attr-defined]
    facilitator.settle_queue.append(
        Answer(
            200,
            {
                "success": True,
                "transaction": "1220uBODY",
                "network": "canton:mainnet",
                "payer": facilitator.payer,
            },
            delay=0.6,
        )
    )
    offer = post_offer(harness)
    harness._offers["/paid"] = offer  # noqa: SLF001  # the harness fetches GET offers; this route is POST
    harness.send_and_leave(1, method="POST", body=b"x" * 150_000)
    assert wait_until(lambda: len(harness.server.registry) == 0, 10)
    assert (
        harness.server.store.claimant("1220uBODY") is None
    )  # the payer left: nothing was delivered, nothing claimed
    retry = harness.client.post(
        "/paid", content=b"y", headers={"PAYMENT-SIGNATURE": harness.header(1)}
    )
    assert retry.status_code == 200  # served once on the retry
    assert facilitator.count("/settle") == 1


def post_offer(harness: Harness) -> dict[str, Any]:
    response = harness.client.post("/paid")
    assert response.status_code == 402
    accepted: dict[str, Any] = json.loads(base64.b64decode(response.headers["payment-required"]))[
        "accepts"
    ][0]
    return accepted


def test_wsgi_drain_reads_only_up_to_the_bound() -> None:
    class Stream:
        def __init__(self) -> None:
            self.read_bytes = 0

        def read(self, size: int = -1) -> bytes:
            self.read_bytes += size
            return b"x" * size

    stream = Stream()
    drain_input({"CONTENT_LENGTH": str(50 * 1024 * 1024), "wsgi.input": stream})
    assert stream.read_bytes == DRAIN_BOUND == 256 * 1024
    short = io.BytesIO(b"abc")
    drain_input({"CONTENT_LENGTH": "3", "wsgi.input": short})
    assert short.read() == b""
    drain_input({"CONTENT_LENGTH": "bad", "wsgi.input": short})  # garbage is ignored, never raised
    drain_input({})


def test_asgi_discard_reads_only_up_to_the_limit_and_reports_overflow() -> None:
    async def run() -> tuple[bool, bool]:
        async def receive() -> dict[str, Any]:
            await asyncio.sleep(0)
            return {"type": "http.request", "body": b"x" * 65536, "more_body": True}

        pump = ReceivePump(receive)
        pump.start()
        finished = await pump.discard_rest(timeout=5, max_bytes=256 * 1024)
        pump.stop()
        return finished, pump.overflow

    assert asyncio.run(run()) == (False, True)


def test_asgi_discard_gives_up_after_the_timeout_when_the_body_never_ends() -> None:
    async def run() -> bool:
        async def receive() -> dict[str, Any]:
            await asyncio.sleep(0.05)
            return {"type": "http.request", "body": b"x", "more_body": True}

        pump = ReceivePump(receive)
        pump.start()
        finished = await pump.discard_rest(timeout=0.3, max_bytes=1 << 30)
        pump.stop()
        return finished

    assert asyncio.run(run()) is False


def test_asgi_discard_finishes_when_the_body_ends_without_back_pressure() -> None:
    async def run() -> bool:
        chunks = iter([{"type": "http.request", "body": b"x" * 100, "more_body": True}] * 40)
        ended = False

        async def receive() -> dict[str, Any]:
            nonlocal ended
            await asyncio.sleep(0)
            try:
                return next(chunks)
            except StopIteration:
                if not ended:
                    ended = True
                    return {"type": "http.request", "body": b"", "more_body": False}
                await asyncio.sleep(3600)  # a real server blocks here until the client leaves
                return {"type": "http.disconnect"}

        pump = ReceivePump(receive)  # 40 chunks are far more than the 8 the pump queues
        pump.start()
        finished = await pump.discard_rest(timeout=5, max_bytes=1 << 20)
        pump.stop()
        return finished and pump.body_complete

    assert asyncio.run(run()) is True


def test_asgi_pump_reports_a_disconnect_and_keeps_serving_it() -> None:
    async def run() -> tuple[bool, str, str]:
        queued: list[dict[str, Any]] = [
            {"type": "http.request", "body": b"a", "more_body": False},
            {"type": "http.disconnect"},
        ]
        messages = iter(queued)

        async def receive() -> dict[str, Any]:
            await asyncio.sleep(0.01)
            return next(messages)

        pump = ReceivePump(receive)
        pump.start()
        first = await pump.receive()
        second = await pump.receive()
        third = await pump.receive()  # the disconnect stays available for every later read
        return pump.gone, str(second["type"]), str(third["type"]) + str(first["body"])

    assert asyncio.run(run()) == (True, "http.disconnect", "http.disconnectb'a'")


# -- a request task cancelled by the server must not cut a settle short --------------------


def test_a_cancelled_request_never_cuts_a_settle_short(facilitator: MockFacilitator) -> None:
    """Hypercorn and others cancel the app task on disconnect; uvicorn does not. Either way the
    settle runs to its end and the payment is recorded, and the body is kept back."""
    store = MemoryStore()
    server = inproc.build_server(facilitator, store=store)
    facilitator.settle_queue.append(
        Answer(
            200,
            {
                "success": True,
                "transaction": "1220uCANCEL",
                "network": "canton:mainnet",
                "payer": facilitator.payer,
            },
            delay=0.5,
        )
    )

    async def run() -> list[MutableMapping[str, Any]]:
        client = await inproc.asgi_client(server, inproc.asgi_app(200, b"PAID"))
        async with client:
            first = await client.get("/paid")
            accepted = json.loads(base64.b64decode(first.headers["payment-required"]))["accepts"][0]
        app = CantonPaymentMiddleware(inproc.asgi_app(200, b"PAID"), server)
        sent: list[MutableMapping[str, Any]] = []

        receive = inproc.idle_receive()

        async def send(message: MutableMapping[str, Any]) -> None:
            sent.append(message)

        scope = {
            "type": "http", "method": "GET", "path": "/paid", "raw_path": b"/paid", "query_string": b"",
            "headers": [(b"payment-signature", payment_header(accepted, tx_hash(1)).encode()), (b"host", b"x")],
            "scheme": "http",
        }  # fmt: skip
        task = asyncio.ensure_future(app(scope, receive, send))
        while facilitator.count("/settle") == 0:
            await asyncio.sleep(0.01)
        task.cancel()  # the server gives up on the request while /settle is in flight
        try:
            await task
        except asyncio.CancelledError:
            pass
        # The settle is still running: the in-flight lock must still be held, or a duplicate
        # of this payment could start a second one.
        assert store.lock_holder(tx_hash(1)) is not None
        for _ in range(300):  # the detached task finishes the settle
            if store.record(tx_hash(1)) is not None and store.record(tx_hash(1)).state == "settled":  # type: ignore[union-attr]
                break
            await asyncio.sleep(0.02)
        await asyncio.sleep(0.1)
        return sent

    sent = asyncio.run(run())
    record = store.record(tx_hash(1))
    assert record is not None and record.state == "settled" and record.update_id == "1220uCANCEL"
    assert (
        store.claimant("1220uCANCEL") is None
    )  # the payer was gone: no claim, so the retry is served once
    assert all(m["type"] != "http.response.body" or b"PAID" not in m.get("body", b"") for m in sent)
    assert (
        len(server.registry) == 0 and store.lock_holder(tx_hash(1)) is None
    )  # the lock was released after the settle
    server.close()


# -- a failed write gives the claim back ----------------------------------------------------


def test_a_failed_asgi_write_releases_the_claim_so_the_retry_is_served_once(
    facilitator: MockFacilitator,
) -> None:
    spy = SpyStore()
    server = inproc.build_server(facilitator, store=spy)

    async def run() -> int:
        client = await inproc.asgi_client(server, inproc.asgi_app(200, b"PAID"))
        async with client:
            first = await client.get("/paid")
            accepted = json.loads(base64.b64decode(first.headers["payment-required"]))["accepts"][0]
            app = CantonPaymentMiddleware(inproc.asgi_app(200, b"PAID"), server)
            receive = inproc.idle_receive()

            async def broken_send(message: MutableMapping[str, Any]) -> None:
                raise BrokenPipeError("client gone")

            scope = {
                "type": "http", "method": "GET", "path": "/paid", "raw_path": b"/paid", "query_string": b"",
                "headers": [(b"payment-signature", payment_header(accepted, tx_hash(1)).encode()), (b"host", b"x")],
                "scheme": "http",
            }  # fmt: skip
            await app(scope, receive, broken_send)  # the write fails after the claim
            assert (
                spy.inner.claimant(facilitator.update_id(tx_hash(1))) is None
            )  # released with the owner token
            retried = await client.get(
                "/paid", headers={"PAYMENT-SIGNATURE": payment_header(accepted, tx_hash(1))}
            )
            return retried.status_code

    assert asyncio.run(run()) == 200
    assert facilitator.count("/settle") == 1
    server.close()


def test_a_failed_wsgi_write_releases_the_claim_so_the_retry_is_served_once(
    facilitator: MockFacilitator,
) -> None:
    spy = SpyStore()
    server = inproc.build_server(facilitator, store=spy)
    app = CantonWSGIMiddleware(inproc.wsgi_app("200 OK", b"PAID"), server)
    with inproc.wsgi_client(server, inproc.wsgi_app("200 OK", b"PAID")) as client:
        accepted = inproc.offer_via(client)
        headers = {"PAYMENT-SIGNATURE": payment_header(accepted, tx_hash(1))}

        def environ() -> dict[str, Any]:
            from werkzeug.test import EnvironBuilder

            return EnvironBuilder(path="/paid", headers=list(headers.items())).get_environ()

        def broken_start_response(
            status: str, response_headers: list[tuple[str, str]], exc_info: Any = None
        ) -> Any:
            def write(data: bytes) -> None:
                raise BrokenPipeError("client gone")

            return write

        result = app(environ(), broken_start_response)
        assert list(result) == []
        assert spy.inner.claimant(facilitator.update_id(tx_hash(1))) is None
        assert client.get("/paid", headers=headers).status_code == 200
    assert facilitator.count("/settle") == 1
    server.close()


def test_a_non_oserror_write_failure_is_raised_but_the_claim_is_still_released(
    facilitator: MockFacilitator,
) -> None:
    spy = SpyStore()
    server = inproc.build_server(facilitator, store=spy)
    app = CantonWSGIMiddleware(inproc.wsgi_app("200 OK", b"PAID"), server)
    with inproc.wsgi_client(server, inproc.wsgi_app("200 OK", b"PAID")) as client:
        accepted = inproc.offer_via(client)
    from werkzeug.test import EnvironBuilder

    environ = EnvironBuilder(
        path="/paid", headers=[("PAYMENT-SIGNATURE", payment_header(accepted, tx_hash(1)))]
    ).get_environ()

    def start_response(
        status: str, response_headers: list[tuple[str, str]], exc_info: Any = None
    ) -> Any:
        def write(data: bytes) -> None:
            raise RuntimeError("server bug")

        return write

    try:
        app(environ, start_response)
    except RuntimeError:
        pass
    else:
        raise AssertionError("expected the RuntimeError")
    assert spy.inner.claimant(facilitator.update_id(tx_hash(1))) is None
    server.close()


# -- merchant HTTP client and shared state -----------------------------------------------------


def test_each_server_has_its_own_memory_store(facilitator: MockFacilitator) -> None:
    first = inproc.build_server(facilitator)
    second = inproc.build_server(facilitator)
    assert first.store is not second.store
    first.close()
    second.close()


def test_a_websocket_on_a_paid_path_is_closed_not_served_free(facilitator: MockFacilitator) -> None:
    server = inproc.build_server(facilitator)
    served: list[str] = []

    async def app(scope: Any, receive: Any, send: Any) -> None:
        served.append(scope["type"])

    middleware = CantonPaymentMiddleware(app, server)
    sent: list[MutableMapping[str, Any]] = []

    async def receive() -> dict[str, Any]:
        return {"type": "websocket.connect"}

    async def send(message: MutableMapping[str, Any]) -> None:
        sent.append(message)

    asyncio.run(
        middleware(
            {"type": "websocket", "path": "/paid", "headers": [], "query_string": b""},
            receive,
            send,
        )
    )
    assert served == [] and sent == [{"type": "websocket.close", "code": 1008}]
    asyncio.run(
        middleware(
            {"type": "websocket", "path": "/free", "headers": [], "query_string": b""},
            receive,
            send,
        )
    )
    assert served == ["websocket"]
    asyncio.run(middleware({"type": "lifespan"}, receive, send))
    assert served == ["websocket", "lifespan"]
    server.close()


def test_a_204_with_a_body_from_a_raw_handler_is_delivered_without_one(
    facilitator: MockFacilitator,
) -> None:
    server = inproc.build_server(facilitator)
    with inproc.wsgi_client(server, inproc.wsgi_app("204 No Content", b"junk")) as client:
        accepted = inproc.offer_via(client)
        response = client.get(
            "/paid", headers={"PAYMENT-SIGNATURE": payment_header(accepted, tx_hash(1))}
        )
        assert response.status_code == 204 and response.content == b""
        assert (
            client.get(
                "/paid", headers={"PAYMENT-SIGNATURE": payment_header(accepted, tx_hash(1))}
            ).status_code
            == 402
        )
    server.close()


def test_a_handler_status_the_server_cannot_buffer_is_not_settled(
    facilitator: MockFacilitator,
) -> None:
    server = inproc.build_server(facilitator)

    async def odd(scope: Any, receive: Any, send: Any) -> None:
        await send({"type": "http.response.start", "status": 200, "headers": []})
        await send({"type": "http.response.pathsend", "path": "/etc/passwd"})

    async def run() -> int:
        client = await inproc.asgi_client(server, inproc.asgi_app(200))
        async with client:
            first = await client.get("/paid")
            accepted = json.loads(base64.b64decode(first.headers["payment-required"]))["accepts"][0]
        wrapped = CantonPaymentMiddleware(odd, server)
        sent: list[MutableMapping[str, Any]] = []

        receive = inproc.idle_receive()

        async def send(message: MutableMapping[str, Any]) -> None:
            sent.append(message)

        scope = {
            "type": "http", "method": "GET", "path": "/paid", "raw_path": b"/paid", "query_string": b"",
            "headers": [(b"payment-signature", payment_header(accepted, tx_hash(1)).encode()), (b"host", b"x")],
            "scheme": "http", "extensions": {"http.response.pathsend": {}},
        }  # fmt: skip
        await wrapped(scope, receive, send)
        return int(sent[0]["status"])

    assert asyncio.run(run()) == 502
    assert facilitator.count("/settle") == 0
    server.close()
