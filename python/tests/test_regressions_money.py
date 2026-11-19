"""Regressions from the Python audit (money lens) and the Go audit rounds 3 and 4."""

from __future__ import annotations

import asyncio
import base64
import json
import threading
import time
from collections.abc import Callable, MutableMapping
from concurrent.futures import ThreadPoolExecutor
from typing import Any

import httpx
import pytest

from ftptech_x402_canton import (
    CantonPaymentMiddleware,
    CantonWSGIMiddleware,
    MemoryStore,
    Route,
    cc,
)
from ftptech_x402_canton.identity import digest_matches, legacy_payment_digest, payment_digest
from tests import inproc
from tests.mock_facilitator import Answer, MockFacilitator, bare, pending
from tests.stores import SpyStore
from tests.support import PAID_ROUTE, PAY_TO, Harness, Out, Seen, tx_hash, wait_until

MSG_UNKNOWN = "payment outcome unknown: do NOT pay again; retry the same payment"


def sleeping_handler(seconds: float) -> Callable[[Seen], Out]:
    def handler(seen: Seen) -> Out:
        time.sleep(seconds)
        return Out()

    return handler


def settled_but_withheld(harness: Harness, facilitator: MockFacilitator, n: int = 1) -> str:
    """Leave payment ``n`` moved but not delivered (its settle answer was lost): the gate holds an
    attempt record, so the reference it names afterwards is the prepared hash."""
    facilitator.settle_always = pending(commit=True)
    assert harness.pay(n).status_code == 503
    facilitator.settle_always = None
    return tx_hash(n)


def settled_and_withheld(
    harness: Harness, spy: SpyStore, facilitator: MockFacilitator, n: int = 1
) -> str:
    """Leave payment ``n`` settled, recorded with its updateId, claim failed: returns the updateId."""
    spy.fail("claim", "raise", "raise", "raise")
    assert harness.pay(n).status_code == 503
    return facilitator.update_id(tx_hash(n))


# -- a claim given up is visible to every instance (audit money 0, Go round 3 and 4) ---------


def test_a_claim_given_up_by_one_instance_is_never_a_final_402_on_another(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    shared = MemoryStore()
    a_store, b_store = SpyStore(shared), SpyStore(shared)
    first, second = make(store=a_store), make(store=b_store)
    # On A: the claim is applied, its reply is lost, the read-back fails, the release fails.
    a_store.fail("claim", "apply_raise")
    a_store.fail("claimant", "raise")
    a_store.fail("unclaim", "raise")
    withheld = first.pay(1)
    assert withheld.status_code == 503 and b'"data"' not in withheld.content
    update_id = facilitator.update_id(tx_hash(1))
    assert shared.claimant(update_id) is not None  # A could not release it

    retry = second.pay(1)  # lands on instance B
    assert retry.status_code == 503  # not the final 402: nothing marks this claim delivered
    assert retry.json() == {"error": MSG_UNKNOWN}
    assert second.receipt(retry)["transaction"] == update_id  # type: ignore[index]

    first.server.ledger.reconcile_once()  # A's background reconciler releases it
    assert shared.claimant(update_id) is None
    served = second.pay(1)
    assert served.status_code == 200 and second.runs == 1
    assert second.pay(1).status_code == 402  # now delivered and marked: the final answer


def test_a_failed_delivery_mark_leaves_replays_at_503_never_402(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    spy = SpyStore()
    harness = make(store=spy)
    spy.fail("mark_delivered", "raise")
    assert harness.pay(1).status_code == 200
    replay = harness.pay(1)
    assert (
        replay.status_code == 503 and harness.runs == 1
    )  # never delivered twice, never the final 402


def test_an_unreadable_claim_holder_on_the_retry_is_503_with_the_update_id(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    spy = SpyStore()
    harness = make(store=spy)
    update_id = settled_and_withheld(harness, spy, facilitator)
    spy.inner.claim(update_id, "elsewhere", 3600)
    spy.fail("claimant", "raise", "raise", "raise")  # the guard's read of the holder fails
    response = harness.pay(1)
    assert response.status_code == 503
    assert harness.receipt(response)["transaction"] == update_id  # type: ignore[index]


# -- the retry of a recorded payment (audit money 2, 3, 4; Go round 3) ----------------------


def test_the_retry_of_a_recorded_payment_is_admitted_on_a_full_memory_store(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    store = MemoryStore(max_entries=10)
    harness = make(store=store)
    facilitator.settle_queue.append(
        Answer(
            200,
            {
                "success": True,
                "transaction": facilitator.update_id(tx_hash(1)),
                "network": "canton:mainnet",
                "payer": facilitator.payer,
            },
            delay=0.5,
        )
    )
    harness.send_and_leave(1)  # settled, withheld: a record, no claim
    assert wait_until(lambda: len(harness.server.registry) == 0, 10)
    for n in range(30):  # the store fills up with other payments' entries
        store.claim(f"filler{n}", "x", 3600)
    fresh = harness.pay(2)
    assert fresh.status_code == 503  # a new payment is refused before anything is settled ...
    assert fresh.json() == {
        "error": "payment state unavailable: nothing was charged; retry the same payment"
    }
    assert harness.pay(1).status_code == 200  # ... the retry of the charged one is served


def test_a_store_error_on_the_retry_names_the_recorded_update_id(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    spy = SpyStore()
    harness = make(store=spy)
    update_id = settled_and_withheld(harness, spy, facilitator)
    spy.fail("lock", "raise", "raise")
    response = harness.pay(1)
    assert response.status_code == 503
    assert harness.receipt(response)["transaction"] == update_id  # type: ignore[index]  # prior is checked first


@pytest.mark.parametrize("answer", ["redirect", "raises", "invalid-status"])
def test_a_handler_that_cannot_serve_the_retry_is_503_do_not_pay_again(
    make: Callable[..., Harness], facilitator: MockFacilitator, answer: str
) -> None:
    spy = SpyStore()
    harness = make(store=spy)
    update_id = settled_and_withheld(harness, spy, facilitator)

    def refuse(seen: Seen) -> Out:
        if answer == "raises":
            raise RuntimeError("order already exists")
        if answer == "redirect":
            return Out(302, b"", {"Location": "/elsewhere"})
        return Out(200)

    harness.handler = refuse
    if answer == "invalid-status":
        pytest.skip("covered in-process below: a real framework refuses to emit an invalid status")
    retry = harness.pay(1)
    assert retry.status_code == 503, retry.text
    assert retry.json() == {"error": MSG_UNKNOWN}
    assert harness.receipt(retry)["transaction"] == update_id  # type: ignore[index]
    assert "location" not in retry.headers
    harness.handler = lambda seen: Out()
    assert harness.pay(1).status_code == 200  # the record was kept: served once


def test_an_invalid_status_on_the_retry_is_503_in_process(facilitator: MockFacilitator) -> None:
    server = inproc.build_server(facilitator)
    facilitator.settle_always = pending(commit=True)
    with inproc.wsgi_client(server, inproc.wsgi_app("200 OK")) as ok_client:
        accepted = inproc.offer_via(ok_client)
        assert ok_client.get("/paid", headers=inproc.header_for(accepted)).status_code == 503
    facilitator.settle_always = None
    with inproc.wsgi_client(server, inproc.wsgi_app("1000 Weird")) as client:
        retry = client.get("/paid", headers=inproc.header_for(accepted))
    assert (
        retry.status_code == 503 and "payment-response" in retry.headers
    )  # (the transport drops write() bodies)
    server.close()


def test_a_raw_wsgi_app_that_raises_on_the_retry_is_503(facilitator: MockFacilitator) -> None:
    server = inproc.build_server(facilitator)
    facilitator.settle_always = pending(commit=True)
    with inproc.wsgi_client(server, inproc.wsgi_app("200 OK")) as client:
        accepted = inproc.offer_via(client)
        assert client.get("/paid", headers=inproc.header_for(accepted)).status_code == 503

    def raising(environ: Any, start_response: Any) -> Any:
        raise RuntimeError("database down")

    facilitator.settle_always = None
    with inproc.wsgi_client(server, raising) as bad:
        retry = bad.get("/paid", headers=inproc.header_for(accepted))
        assert retry.status_code == 503 and retry.json() == {"error": MSG_UNKNOWN}
    with inproc.wsgi_client(server, raising) as bad2, pytest.raises(RuntimeError):
        bad2.get(
            "/paid", headers=inproc.header_for(accepted, 2)
        )  # not a retry: the error is the app's own
    server.close()


def test_the_same_payment_with_other_bytes_after_an_unknown_outcome_is_served(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    """The hash upper-cased or 1220-prefixed is the same payment for the facilitator but other bytes
    for the gate: it used to be 503 for ever. The settled record replaces the attempt."""
    harness = make()
    facilitator.settle_always = pending(commit=True)
    assert harness.pay(1).status_code == 503
    facilitator.settle_always = None
    other_bytes = harness.client.get(
        "/paid", headers={"PAYMENT-SIGNATURE": harness.header(tx=("1220" + tx_hash(1)).upper())}
    )
    assert other_bytes.status_code == 200, other_bytes.text
    assert (
        harness.client.get("/paid", headers={"PAYMENT-SIGNATURE": harness.header(1)}).status_code
        == 402
    )


def test_a_recorded_payment_is_never_answered_with_the_sdks_402_offer(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make()
    update = settled_but_withheld(harness, facilitator)
    facilitator.verify_expired = True  # the deadline passed: /verify refuses other bytes
    other_bytes = harness.client.get(
        "/paid", headers={"PAYMENT-SIGNATURE": harness.header(tx=("1220" + tx_hash(1)).upper())}
    )
    assert other_bytes.status_code == 503 and "payment-required" not in other_bytes.headers
    assert harness.receipt(other_bytes)["transaction"] == "1220" + update  # type: ignore[index]  # the hash as sent
    assert harness.pay(1).status_code == 200  # the very bytes recorded are still served once


# -- the digest (Go round 4) -------------------------------------------------------------------


def test_the_digest_ignores_the_dynamic_extra_fields_and_still_accepts_legacy_records() -> None:
    from x402.schemas import PaymentPayload, PaymentRequirements

    accepted: dict[str, Any] = {
        "scheme": "exact", "network": "canton:mainnet", "asset": "CC", "amount": "1", "payTo": PAY_TO,
        "maxTimeoutSeconds": 120, "extra": {"feePayer": "fee::1", "synchronizerId": "sync::1", "memo": "m"},
    }  # fmt: skip
    payload = PaymentPayload.model_validate(
        {"x402Version": 2, "accepted": accepted, "payload": {"preparedTxHash": "ab" * 32}}
    )
    rotated = PaymentRequirements.model_validate(
        {**accepted, "extra": {"feePayer": "fee::2", "synchronizerId": "sync::2", "memo": "m"}}
    )
    original = PaymentRequirements.model_validate(accepted)
    assert payment_digest(payload, original) == payment_digest(
        payload, rotated
    )  # a fee-payer rotation is not another payment
    assert legacy_payment_digest(payload, original) != legacy_payment_digest(payload, rotated)
    changed = PaymentRequirements.model_validate({**accepted, "amount": "2"})
    assert payment_digest(payload, original) != payment_digest(payload, changed)
    assert digest_matches(
        legacy_payment_digest(payload, original), payload, original
    )  # records written before the change
    assert digest_matches(payment_digest(payload, original), payload, rotated)
    assert not digest_matches("", payload, original) and not digest_matches("x", payload, original)


# -- a request that outlives its lock never shares a token (audit runtime 0) ---------------------


class LossyLockStore(SpyStore):
    """A store whose in-flight lock is lost (it expired): every ``lock`` succeeds."""

    def lock(self, hash: str, token: str, ttl: float) -> bool:
        self.calls.append(("lock", (hash, token, ttl)))
        return True


def test_two_requests_for_one_payment_after_the_lock_is_lost_deliver_once(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    store = LossyLockStore()
    harness = make(store=store)

    def slow(seen: Seen) -> Out:
        time.sleep(0.4)
        return Out()

    harness.handler = slow
    harness.offer()
    barrier = threading.Barrier(2)

    def go(_: int) -> int:
        barrier.wait()
        return harness.pay(1).status_code

    with ThreadPoolExecutor(2) as pool:
        codes = sorted(pool.map(go, range(2)))
    assert codes.count(200) == 1, codes  # one delivery, even though both passed the lock
    assert set(codes) <= {200, 402, 503}
    assert len(harness.server.registry) == 0


def test_each_request_has_its_own_state_and_token_for_one_hash() -> None:
    import logging

    from x402.schemas import PaymentPayload, PaymentRequirements

    from ftptech_x402_canton.facilitator import (
        CantonFacilitator,
        FacilitatorSettings,
        MissingStateError,
    )
    from ftptech_x402_canton.identity import identity_from_payload
    from ftptech_x402_canton.ledger import Ledger
    from ftptech_x402_canton.state import CURRENT_STATE, RequestState, StateRegistry
    from ftptech_x402_canton.transport import HttpAnswer
    from tests.test_facilitator_unit import ACCEPTED, FakeTransport, H

    payload = PaymentPayload.model_validate(
        {"x402Version": 2, "accepted": ACCEPTED, "payload": {"preparedTxHash": H, "signature": "s"}}
    )
    requirements = PaymentRequirements.model_validate(ACCEPTED)
    identity = identity_from_payload(payload)
    assert identity is not None
    state_a = RequestState(identity, "token-A", 100.0, lambda: False)
    state_b = RequestState(identity, "token-B", 100.0, lambda: False)
    registry = StateRegistry()
    registry.add(state_a)
    registry.add(state_b)
    assert len(registry) == 2  # the second never overwrites the first
    ok = HttpAnswer(200, {"success": True, "transaction": "1220uX", "network": "canton:mainnet"})
    spy = SpyStore()
    logger = logging.getLogger("unit")
    facilitator = CantonFacilitator(
        FakeTransport(settle=[ok, ok]),  # type: ignore[arg-type]
        Ledger(spy, logger),
        FacilitatorSettings(backoff=(0.0,), sleep=lambda s: None),
        logger,
    )
    with pytest.raises(MissingStateError):
        facilitator.settle(payload, requirements)  # no request is running this call
    token = CURRENT_STATE.set(state_b)
    try:
        facilitator.settle(payload, requirements)
    finally:
        CURRENT_STATE.reset(token)
    assert state_b.claimed_tx == "1220uX" and state_a.claimed_tx == ""  # B's claim, B's token
    assert spy.inner.claimant("1220uX") == "token-B"
    registry.remove(state_a)
    assert len(registry) == 1


# -- unexpected errors after a /settle call are never 402 (audit money 6) ------------------------


def test_a_client_closed_between_two_settle_calls_is_unknown_not_402(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    class Flaky(httpx.Client):
        settles = 0

        def stream(self, method: str, url: Any, **kwargs: Any) -> Any:
            if str(url).endswith("/settle"):
                Flaky.settles += 1
                if Flaky.settles >= 2:
                    raise RuntimeError("Cannot send a request, as the client has been closed.")
            return super().stream(method, url, **kwargs)

    harness = make(http_client=Flaky())
    facilitator.settle_queue.append(pending())
    response = harness.pay(1)
    assert response.status_code == 503  # never the 402 "settlement_failed" with the exception text
    assert harness.receipt(response)["errorReason"] == "settlement_pending"  # type: ignore[index]
    assert b"closed" not in response.content


def test_an_unexpected_error_after_a_settle_call_returns_unknown() -> None:
    import logging

    from ftptech_x402_canton.facilitator import CantonFacilitator, FacilitatorSettings
    from ftptech_x402_canton.ledger import Ledger
    from ftptech_x402_canton.transport import HttpAnswer
    from tests.test_facilitator_unit import FakeTransport
    from tests.test_facilitator_unit import make as make_unit

    class Exploding(FakeTransport):
        def post(self, path: str, body: bytes, timeout: float) -> HttpAnswer:
            self.posts.append(path)
            if len(self.posts) >= 2:
                raise RuntimeError("boom")
            return HttpAnswer(
                503, {"success": False, "errorReason": "settlement_pending", "transaction": "h"}
            )

    facilitator, state, spy, payload, requirements = make_unit(Exploding())
    del CantonFacilitator, FacilitatorSettings, Ledger, logging
    result = facilitator.settle(payload, requirements)
    assert result.error_reason == "settlement_pending" and state.settle_unknown


def test_the_transport_turns_every_failure_into_no_answer() -> None:
    from ftptech_x402_canton.transport import Transport

    class Broken(httpx.Client):
        def stream(self, *args: Any, **kwargs: Any) -> Any:
            raise RuntimeError("client has been closed")

    answer = Transport("http://x", Broken()).post("/settle", b"{}", 1.0)
    assert answer.status == 0 and answer.error == "RuntimeError"


# -- an attempt older than the facilitator's record (Go round 4) ---------------------------------


def test_a_refusal_for_an_attempt_older_than_the_facilitators_record_stays_unknown() -> None:
    from ftptech_x402_canton.store import STATE_ATTEMPT, Record
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
    digest = payment_digest(payload, requirements)
    old = int(time.time()) - 25 * 3600
    spy.inner.put_record(state.identity.canonical_hash, Record(STATE_ATTEMPT, digest, at=old), 3600)
    result = facilitator.settle(payload, requirements)
    assert result.error_reason == "settlement_pending" and not state.refused
    assert spy.inner.record(state.identity.canonical_hash) is not None  # the attempt stays

    refusal2 = HttpAnswer(
        200,
        {
            "success": False,
            "errorReason": "invalid_exact_canton_expired",
            "transaction": "",
            "network": "canton:mainnet",
        },
    )
    spy2 = SpyStore()
    facilitator2, state2, spy2, payload2, requirements2 = make_unit(
        FakeTransport(settle=[refusal2]), store=spy2
    )
    recent = int(time.time()) - 3600
    spy2.inner.put_record(
        state2.identity.canonical_hash, Record(STATE_ATTEMPT, digest, at=recent), 3600
    )
    result2 = facilitator2.settle(payload2, requirements2)
    assert result2.error_reason == "invalid_exact_canton_expired" and state2.refused
    assert (
        spy2.inner.record(state2.identity.canonical_hash) is None
    )  # a recent attempt is cleared by a refusal


# -- the lock is released before an answer without a claim is written (audit runtime 9) ------------


def test_an_immediate_resend_after_a_gate_answer_is_never_told_in_progress(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make()
    harness.offer()
    for n in range(1, 25):
        facilitator.verify_queue.append(Answer(503, {"error": "scan_busy"}))
        assert harness.pay(n).status_code == 503
        again = harness.pay(n)  # no pause: the client ignored Retry-After
        assert again.status_code == 200, (n, again.text)


# -- the gate sets Content-Length (Go round 3) -----------------------------------------------------


def test_the_gate_owns_content_length_whatever_the_handler_declared(harness: Harness) -> None:
    harness.handler = lambda seen: Out(
        200, b"0123456", {"Content-Type": "text/plain", "Content-Length": "9999"}
    )
    response = harness.pay(1)
    assert (
        response.status_code == 200
        and response.headers["content-length"] == "7"
        and response.text == "0123456"
    )


# -- the reconciler's lifecycle ---------------------------------------------------------------------


def test_the_reconciler_starts_with_the_first_paid_request_and_stops_at_close(
    make: Callable[..., Harness], caplog: pytest.LogCaptureFixture
) -> None:
    harness = make()
    assert (
        not harness.server.reconciler.running
    )  # nothing is started before a paid request (a fork-safe default)
    harness.pay(1)
    assert wait_until(lambda: harness.server.reconciler.running, 3)
    harness.server.close()
    assert not harness.server.reconciler.running
    with pytest.raises(Exception, match="closed"):
        _ = harness.server.executor  # the pool is not recreated after close


def test_close_warns_when_paid_requests_are_still_in_flight(
    make: Callable[..., Harness], caplog: pytest.LogCaptureFixture
) -> None:
    harness = make()
    harness.handler = sleeping_handler(0.8)
    thread = threading.Thread(target=lambda: harness.pay(1), daemon=True)
    harness.offer()
    thread.start()
    assert wait_until(lambda: len(harness.server.registry) == 1, 5)
    harness.server.close(timeout=0.1)
    assert any("closed with paid requests in flight" in r.getMessage() for r in caplog.records)
    thread.join(10)


def test_close_waits_for_paid_requests_in_flight_and_does_not_warn(
    make: Callable[..., Harness], caplog: pytest.LogCaptureFixture
) -> None:
    harness = make()
    harness.handler = sleeping_handler(0.4)
    harness.offer()
    results: list[int] = []
    thread = threading.Thread(
        target=lambda: results.append(harness.pay(1).status_code), daemon=True
    )
    thread.start()
    assert wait_until(lambda: len(harness.server.registry) == 1, 5)
    harness.server.close(timeout=10)
    thread.join(10)
    assert results == [200]
    assert not any("closed with paid requests in flight" in r.getMessage() for r in caplog.records)


def test_a_server_never_closed_logs_what_it_left_behind_at_exit(
    caplog: pytest.LogCaptureFixture,
) -> None:
    import logging

    from ftptech_x402_canton.ledger import Ledger
    from ftptech_x402_canton.reconciler import log_left_behind
    from ftptech_x402_canton.store import Record

    spy = SpyStore()
    ledger = Ledger(spy, logging.getLogger("exit"))
    spy.down = {"put_record"}
    with pytest.raises(ConnectionError):
        ledger.save_settled("h", Record("settled", "d", "u"))
    log_left_behind(ledger, logging.getLogger("exit"))
    assert any("stopped with work left" in r.getMessage() for r in caplog.records)


# -- a mounted app (audit money 5) ---------------------------------------------------------------------


def asgi_call(middleware: Any, scope: dict[str, Any]) -> tuple[int, bytes]:
    sent: list[MutableMapping[str, Any]] = []

    async def receive() -> dict[str, Any]:
        return {"type": "http.request", "body": b"", "more_body": False}

    async def send(message: MutableMapping[str, Any]) -> None:
        sent.append(message)

    asyncio.run(middleware(scope, receive, send))
    return int(sent[0]["status"]), b"".join(
        m.get("body", b"") for m in sent if m["type"] == "http.response.body"
    )


@pytest.mark.parametrize("key", ["GET /api/v1/data", "GET /v1/data"])
@pytest.mark.parametrize("scope_path", ["/api/v1/data", "/v1/data"])
@pytest.mark.parametrize("with_raw_path", [False, True])
def test_a_mounted_app_is_charged_whichever_form_the_key_and_the_server_use(
    facilitator: MockFacilitator, key: str, scope_path: str, with_raw_path: bool
) -> None:
    server = inproc.build_server(facilitator, routes={key: Route(pay_to=PAY_TO, price=cc("0.01"))})
    served: list[int] = []

    async def app(scope: Any, receive: Any, send: Any) -> None:
        served.append(1)
        await send({"type": "http.response.start", "status": 200, "headers": []})
        await send({"type": "http.response.body", "body": b"PAID"})

    scope: dict[str, Any] = {
        "type": "http", "method": "GET", "path": scope_path, "root_path": "/api",
        "query_string": b"", "headers": [(b"host", b"x")], "scheme": "http",
    }  # fmt: skip
    if with_raw_path:
        scope["raw_path"] = scope_path.encode()
    status, body = asgi_call(CantonPaymentMiddleware(app, server), scope)
    # Never served free (Mangum sets no raw_path). A raw_path without the mount prefix next to a root_path
    # is not a combination a server produces; it is refused (404) rather than served.
    assert status in (402, 404) and body != b"PAID" and served == []
    if not (with_raw_path and scope_path == "/v1/data" and key == "GET /api/v1/data"):
        assert status == 402
    server.close()


@pytest.mark.parametrize("key", ["GET /api/v1/data", "GET /v1/data"])
def test_a_wsgi_app_mounted_under_script_name_is_charged(
    facilitator: MockFacilitator, key: str
) -> None:
    server = inproc.build_server(facilitator, routes={key: Route(pay_to=PAY_TO, price=cc("0.01"))})
    served: list[int] = []

    def app(environ: Any, start_response: Any) -> list[bytes]:
        served.append(1)
        start_response("200 OK", [])
        return [b"PAID"]

    status: list[str] = []
    environ = {
        "REQUEST_METHOD": "GET", "SCRIPT_NAME": "/api", "PATH_INFO": "/v1/data", "QUERY_STRING": "",
        "SERVER_NAME": "x", "wsgi.url_scheme": "http", "HTTP_HOST": "x",
    }  # fmt: skip

    def start_response(s: str, h: list[tuple[str, str]], e: Any = None) -> Callable[[bytes], None]:
        status.append(s)
        return lambda data: None

    body = CantonWSGIMiddleware(app, server)(environ, start_response)
    assert status[0].startswith("402") and served == [] and list(body) == []
    server.close()


def test_a_header_with_valid_json_helpers_roundtrip() -> None:
    assert base64.b64decode(base64.b64encode(b"x")) == b"x" and json.loads("{}") == {}
    assert PAID_ROUTE == "GET /paid" and bare(502).status == 502


def test_the_lock_is_released_before_an_answer_without_a_claim_is_written(
    facilitator: MockFacilitator,
) -> None:
    """The claim, not the lock, guards delivery: a 503 or 402 holds no claim, so the lock goes first and a
    client that resends the instant it has read the answer is never told "payment in progress"."""
    import asyncio as aio

    from ftptech_x402_canton import CantonPaymentMiddleware
    from tests.support import payment_header

    spy = SpyStore()
    server = inproc.build_server(facilitator, store=spy)
    held_when_written: list[Any] = []

    async def run() -> None:
        client = await inproc.asgi_client(server, inproc.asgi_app(200))
        async with client:
            first = await client.get("/paid")
        accepted = json.loads(base64.b64decode(first.headers["payment-required"]))["accepts"][0]
        facilitator.verify_queue.append(Answer(503, {"error": "scan_busy"}))
        middleware = CantonPaymentMiddleware(inproc.asgi_app(200), server)

        async def receive() -> dict[str, Any]:
            return {"type": "http.request", "body": b"", "more_body": False}

        async def send(message: MutableMapping[str, Any]) -> None:
            if message["type"] == "http.response.start":
                held_when_written.append((message["status"], spy.inner.lock_holder(tx_hash(1))))

        scope = {
            "type": "http", "method": "GET", "path": "/paid", "raw_path": b"/paid", "query_string": b"",
            "headers": [(b"payment-signature", payment_header(accepted, tx_hash(1)).encode()), (b"host", b"x")],
            "scheme": "http",
        }  # fmt: skip
        await middleware(scope, receive, send)

    aio.run(run())
    assert held_when_written == [(503, None)]  # written with no lock held
    server.close()
