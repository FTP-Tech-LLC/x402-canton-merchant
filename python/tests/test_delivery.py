"""Rule 10: one delivery per payment."""

from __future__ import annotations

import base64
import json
import random
import threading
import time
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor
from typing import Any

import httpx

from ftptech_x402_canton import MemoryStore, RedisStore, request_budget
from ftptech_x402_canton.store import RECORD_TTL_SECONDS, STATE_ATTEMPT, STATE_SETTLED
from tests.mock_facilitator import Answer, MockFacilitator, bare, pending
from tests.stores import FakeRedis, SpyStore
from tests.support import Harness, Out, Seen, payment_header, tx_hash, wait_until

MSG_UNKNOWN = "payment outcome unknown: do NOT pay again; retry the same payment"


def canonical(n: int) -> str:
    return tx_hash(n)


def test_replay_inside_the_deadline_is_refused_before_verify_and_runs_no_handler(
    harness: Harness, facilitator: MockFacilitator
) -> None:
    first = harness.pay(1)
    assert first.status_code == 200
    update_id = harness.receipt(first)["transaction"]  # type: ignore[index]
    verifies, settles = facilitator.count("/verify"), facilitator.count("/settle")
    for _ in range(3):
        replay = harness.pay(1)
        assert replay.status_code == 402
        assert replay.json() == {"error": "payment_already_redeemed"}
        assert harness.receipt(replay) == {
            "success": False, "errorReason": "payment_already_redeemed", "transaction": update_id,
            "network": "canton:mainnet",
        }  # fmt: skip
    assert harness.runs == 1
    assert (facilitator.count("/verify"), facilitator.count("/settle")) == (verifies, settles)


def test_different_spellings_of_one_hash_are_one_payment(harness: Harness) -> None:
    plain = canonical(7)
    assert (
        harness.client.get(
            "/paid", headers={"PAYMENT-SIGNATURE": harness.header(tx=plain)}
        ).status_code
        == 200
    )
    for spelling in (plain.upper(), "1220" + plain, "  " + plain + "  ", ("1220" + plain).upper()):
        replay = harness.client.get(
            "/paid", headers={"PAYMENT-SIGNATURE": harness.header(tx=spelling)}
        )
        assert replay.status_code == 402, spelling
    assert harness.runs == 1


def test_concurrent_duplicates_run_one_handler_and_one_settle(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make()

    def slow(seen: Seen) -> Out:
        time.sleep(0.4)
        return Out()

    harness.handler = slow
    harness.offer()
    barrier = threading.Barrier(8)

    def go(_: int) -> httpx.Response:
        barrier.wait()
        return harness.pay(1)

    with ThreadPoolExecutor(8) as pool:
        responses = list(pool.map(go, range(8)))
    statuses = sorted(r.status_code for r in responses)
    assert statuses.count(200) == 1
    assert all(code in (200, 402, 503) for code in statuses)
    for response in responses:
        if response.status_code == 503:
            assert response.json() == {"error": "payment in progress: retry the same payment"}
            assert response.headers["retry-after"] == "3"
    assert harness.runs == 1
    assert facilitator.count("/settle") == 1


def test_racing_duplicates_never_deliver_twice(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make()
    harness.offer()
    rng = random.Random(7)
    for round_number in range(6):
        number = 100 + round_number
        delays = [rng.random() * 0.02 for _ in range(10)]

        def go(delay: float, n: int = number) -> int:
            time.sleep(delay)
            return harness.pay(n).status_code

        with ThreadPoolExecutor(10) as pool:
            codes = list(pool.map(go, delays))
        assert codes.count(200) == 1, (round_number, codes)
    assert facilitator.count("/settle") == 6


def test_the_lock_is_released_when_the_request_ends(make: Callable[..., Harness]) -> None:
    spy = SpyStore()
    harness = make(store=spy)
    harness.handler = lambda seen: Out(500, b"no")
    harness.pay(1)
    assert spy.inner.lock_holder(canonical(1)) is None
    unlocks = spy.args("unlock")
    locks = spy.args("lock")
    assert unlocks and unlocks[-1][1] == locks[0][1]  # released with the owner token


def test_settled_and_delivered_records_are_written_with_the_documented_ttls(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    spy = SpyStore()
    harness = make(store=spy)
    response = harness.pay(3)
    update_id = harness.receipt(response)["transaction"]  # type: ignore[index]
    record = spy.inner.record(canonical(3))
    assert record is not None and record.state == STATE_SETTLED
    assert (
        record.update_id == update_id
        and record.payer == facilitator.payer
        and len(record.digest) == 64
    )
    holder = spy.inner.claimant(update_id)
    # The claim is owned by the request's token and, once the response was written, marked delivered.
    assert holder == "delivered:" + spy.args("lock")[0][1]
    names = ("put_record", "claim", "mark_delivered")
    ttls = {name: args[-1] for name, args in spy.calls if name in names}
    assert ttls == dict.fromkeys(names, RECORD_TTL_SECONDS)
    assert RECORD_TTL_SECONDS == 26 * 3600


def test_lock_ttl_follows_the_matched_route(make: Callable[..., Harness]) -> None:
    """The in-flight lock lives as long as one paid request on that route can: the
    larger of ``timeout`` and the route's lifetime, every settle call, the pauses, the store calls."""
    from ftptech_x402_canton import Route, cc
    from tests.support import PAY_TO

    spy = SpyStore()
    routes = {
        "GET /paid": Route(pay_to=PAY_TO, price=cc("0.01"), execute_before_seconds=30),
        "GET /other": Route(pay_to=PAY_TO, price=cc("0.01"), execute_before_seconds=300),
    }
    harness = make(store=spy, routes=routes, timeout=10.0)
    harness.pay(1)
    tail = 4 * 5.0 + 3 * 0.02 + 7 * 10.0  # settle calls + pauses + store calls (FAST config)
    assert abs(spy.args("lock")[0][2] - (30.0 + tail)) < 1e-6
    assert (
        request_budget(harness.config) == 300.0 + tail
    )  # the config-wide figure uses the longest route


def test_request_budget_is_the_documented_formula() -> None:
    from ftptech_x402_canton import Config, Route, cc
    from tests.support import PAY_TO

    config = Config(routes={"GET /x": Route(pay_to=PAY_TO, price=cc("1"))})
    # lifetime 120 + 4 settle calls of 65 s + 3 pauses of 5 s + 7 store calls of 10 s
    assert request_budget(config) == 120 + 4 * 65 + 3 * 5 + 7 * 10


def test_settled_but_undelivered_is_served_after_the_deadline_without_verify(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make()
    facilitator.settle_always = pending(commit=True)
    first = harness.pay(1)
    assert first.status_code == 503
    assert facilitator.count("/settle") == 4  # the whole budget
    facilitator.settle_always = None
    facilitator.verify_expired = True  # the deadline has passed: /verify would refuse
    verifies = facilitator.count("/verify")
    retry = harness.pay(1)
    assert retry.status_code == 200, retry.text
    assert facilitator.count("/verify") == verifies  # /verify was skipped for the recorded bytes
    assert harness.runs == 2
    assert harness.pay(1).status_code == 402  # served once


def test_concurrent_retries_of_a_settled_but_undelivered_payment_serve_once(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make()
    facilitator.settle_always = pending(commit=True)
    assert harness.pay(1).status_code == 503
    facilitator.settle_always = None
    facilitator.verify_expired = True
    barrier = threading.Barrier(6)

    def go(_: int) -> int:
        barrier.wait()
        return harness.pay(1).status_code

    with ThreadPoolExecutor(6) as pool:
        codes = list(pool.map(go, range(6)))
    assert codes.count(200) == 1, codes
    assert set(codes) <= {200, 402, 503}


def test_already_settled_is_delivered_once_with_the_original_update_id(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make()
    facilitator.settle_queue.extend([bare(502)] * 4)  # the answer is lost ...
    facilitator.settled[canonical(1)] = "1220uORIGINAL"  # ... but the payment moved
    assert harness.pay(1).status_code == 503
    retry = harness.pay(1)  # the facilitator now answers already_settled
    assert retry.status_code == 200
    assert harness.receipt(retry)["transaction"] == "1220uORIGINAL"  # type: ignore[index]
    again = harness.pay(1)
    assert again.status_code == 402
    assert harness.receipt(again)["transaction"] == "1220uORIGINAL"  # type: ignore[index]
    assert harness.runs == 2  # once lost, once served


def test_already_settled_without_a_transaction_is_unknown_not_a_refusal(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    """The facilitator says the payment moved but names no updateId: nothing to deliver, nothing
    proven. Not a 402 refusal: unknown, retried within the budget, and the attempt stays."""
    spy = SpyStore()
    harness = make(store=spy)
    facilitator.settle_always = Answer(
        200,
        {
            "success": False,
            "errorReason": "invalid_exact_canton_payment_already_settled",
            "transaction": "",
            "network": "canton:mainnet",
        },
    )
    response = harness.pay(1)
    assert response.status_code == 503
    assert response.json() == {"error": MSG_UNKNOWN}
    assert facilitator.count("/settle") == 4
    record = spy.inner.record(canonical(1))
    assert record is not None and record.state == STATE_ATTEMPT  # kept: a retry can skip /verify


def test_already_settled_whose_update_id_was_delivered_stays_402_redeemed(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    spy = SpyStore()
    harness = make(store=spy)
    facilitator.settled[canonical(1)] = "1220uTAKEN"
    spy.inner.claim("1220uTAKEN", "someone-else", 3600)
    spy.inner.mark_delivered("1220uTAKEN", "someone-else", 3600)  # confirmed delivered
    response = harness.pay(1)
    assert response.status_code == 402
    assert response.json() == {"error": "payment_already_redeemed"}
    assert harness.receipt(response)["transaction"] == "1220uTAKEN"  # type: ignore[index]


def test_a_claim_of_another_request_that_is_not_marked_is_503_never_the_final_402(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    """Its owner may be mid-delivery, may have given up, or may have died: the payer retries."""
    spy = SpyStore()
    harness = make(store=spy)
    facilitator.settled[canonical(1)] = "1220uTAKEN"
    spy.inner.claim("1220uTAKEN", "someone-else", 3600)
    response = harness.pay(1)
    assert response.status_code == 503
    assert response.json() == {"error": MSG_UNKNOWN}
    assert harness.receipt(response)["transaction"] == "1220uTAKEN"  # type: ignore[index]
    assert b'"data"' not in response.content
    spy.inner.unclaim("1220uTAKEN", "someone-else")  # the other request gave up and released
    assert harness.pay(1).status_code == 200


def test_success_whose_update_id_was_already_delivered_is_aborted(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    spy = SpyStore()
    harness = make(store=spy)
    update_id = facilitator.update_id(canonical(1))
    spy.inner.claim(update_id, "someone-else", 3600)
    spy.inner.mark_delivered(update_id, "someone-else", 3600)
    response = harness.pay(1)
    assert response.status_code == 402 and response.json() == {"error": "payment_already_redeemed"}
    assert b'"data"' not in response.content  # the paid body is never delivered


def test_success_without_a_transaction_is_not_delivered(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make()
    facilitator.settle_always = Answer(
        200, {"success": True, "transaction": "", "network": "canton:mainnet"}
    )
    response = harness.pay(1)
    assert response.status_code == 503 and response.json() == {"error": MSG_UNKNOWN}
    assert b'"data"' not in response.content


def test_store_failure_on_claim_withholds_the_body_and_the_retry_is_served_once(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    spy = SpyStore()
    harness = make(store=spy)
    spy.fail("claim", "raise", "raise", "raise")
    response = harness.pay(1)
    assert response.status_code == 503
    assert response.json() == {"error": MSG_UNKNOWN}
    receipt = harness.receipt(response)
    assert receipt["errorReason"] == "settlement_pending"  # type: ignore[index]
    assert receipt["transaction"] == facilitator.update_id(canonical(1))  # type: ignore[index]
    assert b'"data"' not in response.content
    assert spy.inner.claimant(facilitator.update_id(canonical(1))) is None
    retry = harness.pay(1)
    assert retry.status_code == 200
    assert (
        facilitator.count("/settle") == 1
    )  # the recorded updateId is delivered: no second /settle
    assert harness.pay(1).status_code == 402


def test_store_failure_on_the_settled_record_still_retries_in_process(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    spy = SpyStore()
    harness = make(store=spy)
    spy.fail(
        "put_record", "ok", "raise", "raise"
    )  # the attempt is stored, the settled record is not
    first = harness.pay(1)
    assert first.status_code == 503
    assert harness.receipt(first)["transaction"] == facilitator.update_id(canonical(1))  # type: ignore[index]
    assert b'"data"' not in first.content
    facilitator.verify_expired = True
    retry = harness.pay(1)  # served from the in-process fallback record, past the deadline
    assert retry.status_code == 200


def test_store_failure_on_the_recovery_of_already_settled_is_503(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    spy = SpyStore()
    harness = make(store=spy)
    facilitator.settled[canonical(1)] = "1220uORIGINAL"
    spy.fail("claim", "raise", "raise", "raise")
    response = harness.pay(1)
    assert response.status_code == 503  # never a 402: the payment moved
    assert harness.receipt(response)["transaction"] == "1220uORIGINAL"  # type: ignore[index]


def test_store_down_before_verify_is_503_and_nothing_is_settled(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    spy = SpyStore()
    harness = make(store=spy)
    offer = harness.offer()
    spy.down_all()
    response = harness.client.get(
        "/paid", headers={"PAYMENT-SIGNATURE": payment_header(offer, canonical(1))}
    )
    assert response.status_code == 503
    assert (
        response.headers["retry-after"] == "3" and response.headers["cache-control"] == "no-store"
    )
    assert facilitator.count("/verify") == 0 and facilitator.count("/settle") == 0
    assert harness.runs == 0
    spy.up()
    assert harness.pay(1).status_code == 200


def test_a_full_memory_store_refuses_before_anything_is_settled(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make(store=MemoryStore(max_entries=3))
    assert harness.pay(1).status_code == 200  # lock + record + claim fit; the lock is then released
    response = harness.pay(2)
    assert response.status_code == 503
    assert response.json() == {
        "error": "payment state unavailable: nothing was charged; retry the same payment"
    }
    assert facilitator.count("/settle") == 1
    assert harness.runs == 1


def test_two_instances_share_one_delivery_through_redis(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    shared = FakeRedis()
    first = make(store=RedisStore(shared, "x402:test:"))
    second = make(store=RedisStore(shared, "x402:test:"))
    assert first.pay(1).status_code == 200
    replay = second.pay(1)
    assert replay.status_code == 402 and replay.json() == {"error": "payment_already_redeemed"}
    assert second.runs == 0
    assert all(key.startswith("x402:test:") for _, key in shared.ops)


def slow_handler(seen: Seen) -> Out:
    time.sleep(0.15)
    return Out()


def test_two_instances_racing_deliver_once(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    shared = FakeRedis()
    first = make(store=RedisStore(shared, "x402:race:"))
    second = make(store=RedisStore(shared, "x402:race:"))
    for harness in (first, second):
        harness.handler = slow_handler
        harness.offer()
    barrier = threading.Barrier(2)

    def go(harness: Harness) -> int:
        barrier.wait()
        return harness.pay(5).status_code

    with ThreadPoolExecutor(2) as pool:
        codes = list(pool.map(go, (first, second)))
    assert sorted(codes)[0] in (200,) and codes.count(200) == 1
    assert facilitator.count("/settle") == 1


def test_a_redis_outage_before_verify_is_503(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    redis = FakeRedis()
    harness = make(store=RedisStore(redis))
    offer = harness.offer()
    redis.error = ConnectionError("redis down")
    response = harness.client.get(
        "/paid", headers={"PAYMENT-SIGNATURE": payment_header(offer, canonical(1))}
    )
    assert response.status_code == 503
    assert facilitator.count("/verify") == 0


def test_an_unparsable_header_goes_to_the_sdk_without_a_lock(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    spy = SpyStore()
    harness = make(store=spy)
    for junk in (
        "!!!not base64!!!",
        base64.b64encode(b"not json").decode(),
        base64.b64encode(b"[1,2]").decode(),
    ):
        response = harness.client.get("/paid", headers={"PAYMENT-SIGNATURE": junk})
        assert response.status_code == 402
    assert spy.calls == []  # no lock, no read
    assert facilitator.count("/verify") == 0


def raw_header(accepted: dict[str, Any], payload: dict[str, Any]) -> str:
    body = {"x402Version": 2, "accepted": accepted, "payload": payload}
    return base64.b64encode(json.dumps(body).encode()).decode()


def test_a_header_without_a_usable_hash_is_a_400_before_any_lock_or_verify(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    spy = SpyStore()
    harness = make(store=spy)
    offer = harness.offer()
    for payload in (
        {"signature": "x"},
        {"preparedTxHash": 12345},
        {"preparedTxHash": "short"},
        {"preparedTxHash": "zz" * 32},
        {"PreparedTxHash": tx_hash(1)},  # another spelling of the key is not the key
        {"preparedtxhash": tx_hash(1)},
    ):
        response = harness.client.get(
            "/paid", headers={"PAYMENT-SIGNATURE": raw_header(offer, payload)}
        )
        assert response.status_code == 400, payload
        assert response.json() == {"error": "payment header carries no valid preparedTxHash"}
    assert spy.calls == [] and facilitator.count("/verify") == 0


def test_identity_is_read_by_the_exact_key_like_the_sdk(
    harness: Harness, facilitator: MockFacilitator
) -> None:
    offer = harness.offer()
    header = raw_header(
        offer, {"preparedTxHash": tx_hash(9), "PreparedTxHash": tx_hash(8), "signature": "x"}
    )
    assert harness.client.get("/paid", headers={"PAYMENT-SIGNATURE": header}).status_code == 200
    shadow = raw_header(
        offer, {"preparedTxHash": tx_hash(9), "PreparedTxHash": tx_hash(7), "signature": "y"}
    )
    replay = harness.client.get("/paid", headers={"PAYMENT-SIGNATURE": shadow})
    assert replay.status_code == 402 and replay.json() == {"error": "payment_already_redeemed"}
    assert harness.runs == 1
    sent = facilitator.calls_to("/settle")[0].json()["paymentPayload"]["payload"]
    assert sent["preparedTxHash"] == tx_hash(9)  # the facilitator reads the same key


def test_prefixed_and_upper_case_hashes_still_work(
    harness: Harness, facilitator: MockFacilitator
) -> None:
    response = harness.client.get(
        "/paid", headers={"PAYMENT-SIGNATURE": harness.header(tx=("1220" + tx_hash(4)).upper())}
    )
    assert response.status_code == 200
    assert harness.receipt(response)["transaction"]  # type: ignore[index]


def test_the_paid_body_is_never_delivered_on_any_failure(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make()
    scripted = [
        pending(), bare(500), bare(502), bare(429),
        Answer(200, {"success": False, "errorReason": "invalid_exact_canton_expired", "transaction": "",
                     "network": "canton:mainnet"}),
    ]  # fmt: skip
    for number, answer in enumerate(scripted, start=20):
        facilitator.settle_always = answer
        response = harness.pay(number)
        assert response.status_code in (402, 503), response.status_code
        assert b'"data"' not in response.content
        assert harness.receipt(response) is not None


def test_a_retry_whose_handler_answers_4xx_is_503_not_a_bare_4xx(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    """The retry of a payment that may have moved must not hide the charge behind a 429."""
    harness = make()
    facilitator.settle_always = pending(commit=True)
    assert harness.pay(1).status_code == 503
    facilitator.settle_always = None
    harness.handler = lambda seen: Out(429, b'{"error":"limit"}')
    retry = harness.pay(1)
    assert retry.status_code == 503
    assert retry.json() == {"error": MSG_UNKNOWN}
    assert harness.seen[-1].retry is True
    harness.handler = lambda seen: Out()
    assert harness.pay(1).status_code == 200  # the record was kept: still served once


def test_the_attempt_is_recorded_before_the_first_settle_call(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    spy = SpyStore()
    harness = make(store=spy)
    seen: list[Any] = []
    facilitator.on_call = lambda call: (
        seen.append(spy.inner.record(canonical(1))) if call.path == "/settle" else None
    )
    harness.pay(1)
    assert seen and seen[0] is not None and seen[0].state == STATE_ATTEMPT


def test_a_definite_refusal_clears_the_attempt(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    spy = SpyStore()
    harness = make(store=spy)
    facilitator.settle_always = Answer(
        200,
        {
            "success": False,
            "errorReason": "invalid_exact_canton_expired",
            "transaction": "",
            "network": "canton:mainnet",
        },
    )
    assert harness.pay(1).status_code == 402
    assert (
        spy.inner.record(canonical(1)) is None
    )  # nothing moved: a retry goes through /verify again
    facilitator.settle_always = None
    verifies = facilitator.count("/verify")
    assert harness.pay(1).status_code == 200
    assert facilitator.count("/verify") == verifies + 1


def test_the_attempt_survives_a_store_write_failure_in_process(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    spy = SpyStore()
    harness = make(store=spy)
    spy.fail("put_record", "raise")  # the attempt write fails
    facilitator.settle_always = pending(commit=True)
    assert (
        harness.pay(1).status_code == 503
    )  # settle went on: the payer is not refused for our store
    facilitator.settle_always = None
    facilitator.verify_expired = True
    verifies = facilitator.count("/verify")
    assert harness.pay(1).status_code == 200  # the fallback record let the retry skip /verify
    assert facilitator.count("/verify") == verifies


def test_a_retry_after_a_crash_mid_settle_is_served(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    """A new process with the same shared store: the attempt record lets the retry skip /verify."""
    shared = FakeRedis()
    first = make(store=RedisStore(shared, "x402:crash:"))
    facilitator.settle_always = pending(commit=True)
    assert first.pay(1).status_code == 503
    facilitator.settle_always = None
    facilitator.verify_expired = True
    second = make(store=RedisStore(shared, "x402:crash:"))
    assert second.pay(1).status_code == 200
    assert second.pay(1).status_code == 402


def test_skip_verify_only_for_the_very_bytes_that_were_attempted(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make()
    facilitator.settle_always = pending(commit=True)
    assert harness.pay(1).status_code == 503
    facilitator.settle_always = None
    verifies = facilitator.count("/verify")
    # Same hash, different signature: another set of bytes. /verify decides.
    other = harness.client.get(
        "/paid", headers={"PAYMENT-SIGNATURE": harness.header(tx=tx_hash(1), signature="different")}
    )
    assert facilitator.count("/verify") == verifies + 1
    assert other.status_code in (200, 402, 503)
    assert wait_until(lambda: True)
