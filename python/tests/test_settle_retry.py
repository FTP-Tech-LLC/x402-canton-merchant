"""Rules 7, 8, 9: settle retries, the unknown outcome, /verify outages."""

from __future__ import annotations

import time
from collections.abc import Callable

import pytest

from ftptech_x402_canton.config import (
    DEFAULT_BACKOFF,
    DEFAULT_RETRY_AFTER_CAP,
    DEFAULT_SETTLE_CALLS,
    DEFAULT_SETTLE_TIMEOUT,
)
from ftptech_x402_canton.facilitator import FacilitatorSettings
from tests.mock_facilitator import Answer, MockFacilitator, bare, pending, refuse
from tests.support import Harness, tx_hash

MSG_UNKNOWN = "payment outcome unknown: do NOT pay again; retry the same payment"
MSG_VERIFY = "verification unavailable: nothing was settled; retry the same payment"

UNKNOWN_KINDS = {
    "pending-503": pending(),
    "pending-200": Answer(200, {"success": False, "errorReason": "settlement_pending", "transaction": "", "network": "canton:mainnet"}),
    "429": Answer(429, {"error": "rate_limited"}, {"Retry-After": "1"}),
    "bare-502": bare(502),
    "bare-500": bare(500),
    "bare-400": bare(400),
    "503-no-body": bare(503),
    "dropped-connection": Answer(drop=True),
    "garbage-200": Answer(200, raw=b"<html>proxy error</html>"),
    "200-without-reason": Answer(200, {"success": False, "transaction": "", "network": "canton:mainnet"}),
    "scan-busy": Answer(503, {"error": "scan_busy"}, {"Retry-After": "3"}),
}  # fmt: skip


@pytest.mark.parametrize("name", sorted(UNKNOWN_KINDS))
def test_every_unknown_outcome_kind_is_retried_then_succeeds(
    make: Callable[..., Harness], facilitator: MockFacilitator, name: str
) -> None:
    harness = make()
    facilitator.settle_queue.append(UNKNOWN_KINDS[name])
    response = harness.pay(1)
    assert response.status_code == 200, (name, response.text)
    assert facilitator.count("/settle") == 2
    assert harness.runs == 1


def test_pending_then_success_is_delivered_once_with_identical_bytes(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make()
    facilitator.settle_queue.extend([pending(), pending()])
    assert harness.pay(1).status_code == 200
    settles = facilitator.calls_to("/settle")
    assert len(settles) == 3
    assert len({call.body for call in settles}) == 1  # the identical bytes every time
    assert settles[0].body == facilitator.calls_to("/verify")[0].body  # and the bytes /verify saw
    assert harness.pay(1).status_code == 402  # one delivery


@pytest.mark.parametrize(
    ("retry_after", "expected"),
    [("3", 3.0), ("1", 1.0), ("30", 5.0), ("0", 1.0), ("soon", 1.0)],
)
def test_retry_after_is_honoured_and_capped_at_five_seconds(
    make: Callable[..., Harness], facilitator: MockFacilitator, retry_after: str, expected: float
) -> None:
    delays: list[float] = []
    harness = make(
        sleep=delays.append, retry_after_cap=DEFAULT_RETRY_AFTER_CAP, settle_backoff=DEFAULT_BACKOFF
    )
    facilitator.settle_queue.append(
        Answer(
            503,
            {
                "success": False,
                "errorReason": "settlement_pending",
                "transaction": "",
                "network": "canton:mainnet",
            },
            {"Retry-After": retry_after},
        )
    )
    assert harness.pay(1).status_code == 200
    assert delays == [expected]


def test_without_retry_after_the_wait_is_one_two_then_four_seconds(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    delays: list[float] = []
    harness = make(
        sleep=delays.append, settle_backoff=DEFAULT_BACKOFF, retry_after_cap=DEFAULT_RETRY_AFTER_CAP
    )
    facilitator.settle_queue.extend([bare(502)] * 3)
    assert harness.pay(1).status_code == 200
    assert delays == [1.0, 2.0, 4.0]


def test_production_defaults() -> None:
    settings = FacilitatorSettings()
    assert settings.retry_after_cap == 5.0 and tuple(settings.backoff) == (1.0, 2.0, 4.0)
    assert settings.settle_calls == DEFAULT_SETTLE_CALLS == 4
    assert (
        settings.settle_timeout == DEFAULT_SETTLE_TIMEOUT == 65.0
    )  # one /settle can take about 57 s


@pytest.mark.parametrize(("calls", "expected"), [(4, 4), (2, 2), (1, 1)])
def test_the_settle_budget_counts_the_sdks_own_retry(
    make: Callable[..., Harness], facilitator: MockFacilitator, calls: int, expected: int
) -> None:
    """Pending forever: the SDK retries a pending answer once itself. That retry must
    not add a call beyond the budget."""
    harness = make(settle_calls=calls)
    facilitator.settle_always = pending()
    response = harness.pay(1)
    assert response.status_code == 503
    assert facilitator.count("/settle") == expected


def test_the_budget_is_per_request_not_per_process(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make(settle_calls=2)
    facilitator.settle_always = pending()
    for number in (1, 2, 3):
        assert harness.pay(number).status_code == 503
    assert facilitator.count("/settle") == 6


def test_a_definite_answer_returns_immediately_and_keeps_the_reason(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make()
    facilitator.settle_always = refuse("invalid_exact_canton_expired")
    response = harness.pay(1)
    assert response.status_code == 402
    assert facilitator.count("/settle") == 1
    assert harness.receipt(response)["errorReason"] == "invalid_exact_canton_expired"  # type: ignore[index]
    assert b'"data"' not in response.content


def test_a_slow_settle_outlives_the_verify_deadline_and_the_http_clients_timeout(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    import httpx

    client = httpx.Client(timeout=0.2)  # a merchant client with a short timeout of its own
    harness = make(verify_timeout=0.3, http_client=client)
    facilitator.settle_queue.append(
        Answer(
            200,
            {
                "success": True,
                "transaction": "1220uSLOW",
                "network": "canton:mainnet",
                "payer": facilitator.payer,
            },
            delay=0.7,
        )
    )
    response = harness.pay(1)
    assert response.status_code == 200
    assert harness.receipt(response)["transaction"] == "1220uSLOW"  # type: ignore[index]
    assert facilitator.count("/settle") == 1
    assert client.timeout == httpx.Timeout(0.2) and not client.is_closed  # not mutated, not closed
    client.close()


def test_settle_timeout_applies_per_call(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make(settle_timeout=0.3, settle_calls=2)
    facilitator.settle_always = Answer(
        200, {"success": True, "transaction": "1220uLATE", "network": "canton:mainnet"}, delay=1.0
    )
    started = time.monotonic()
    response = harness.pay(1)
    assert response.status_code == 503  # each call timed out: the outcome is unknown
    assert facilitator.count("/settle") == 2
    assert time.monotonic() - started < 2.5


def test_unknown_outcome_after_the_budget_is_503_do_not_pay_again(
    make: Callable[..., Harness], facilitator: MockFacilitator, caplog: pytest.LogCaptureFixture
) -> None:
    harness = make()
    facilitator.settle_always = pending()
    response = harness.pay(1)
    assert response.status_code == 503  # never 402
    assert response.headers["retry-after"] == "3"
    assert response.headers["cache-control"] == "no-store"
    assert response.headers["content-type"].startswith("application/json")
    assert response.json() == {"error": MSG_UNKNOWN}
    assert harness.receipt(response) == {
        "success": False, "errorReason": "settlement_pending", "transaction": tx_hash(1), "network": "canton:mainnet",
    }  # fmt: skip
    assert b'"data"' not in response.content
    warnings = [r for r in caplog.records if "outcome unknown" in r.getMessage()]
    assert (
        len(warnings) == 1 and tx_hash(1) in warnings[0].getMessage()
    )  # logged once, with the canonical hash


def test_unknown_outcome_receipt_names_the_hash_as_sent_lower_cased(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make()
    facilitator.settle_always = pending()
    spelled = ("1220" + tx_hash(1)).upper()
    response = harness.client.get(
        "/paid", headers={"PAYMENT-SIGNATURE": harness.header(tx=spelled)}
    )
    assert harness.receipt(response)["transaction"] == "1220" + tx_hash(1)  # type: ignore[index]


@pytest.mark.parametrize(
    "answer",
    [
        Answer(429, {"error": "rate_limited"}),
        Answer(503, {"error": "scan_busy"}, {"Retry-After": "3"}),
        Answer(500, {"error": "boom"}),
        bare(502),
        Answer(drop=True),
        Answer(200, raw=b"garbage"),
        Answer(200, {"unexpected": True}),
    ],
    ids=["429", "503", "500", "bare-502", "dropped", "garbage", "no-verdict"],
)
def test_verify_outages_are_503_not_402(
    make: Callable[..., Harness], facilitator: MockFacilitator, answer: Answer
) -> None:
    harness = make()
    offer = harness.offer()
    facilitator.verify_queue.append(answer)
    from tests.support import payment_header

    response = harness.client.get(
        "/paid", headers={"PAYMENT-SIGNATURE": payment_header(offer, tx_hash(1))}
    )
    assert response.status_code == 503
    assert response.json() == {"error": MSG_VERIFY}
    assert (
        response.headers["retry-after"] == "3" and response.headers["cache-control"] == "no-store"
    )
    assert harness.runs == 0 and facilitator.count("/settle") == 0
    assert "payment-required" not in response.headers


def test_verify_timeout_is_503(make: Callable[..., Harness], facilitator: MockFacilitator) -> None:
    harness = make(verify_timeout=0.2)
    harness.offer()
    facilitator.verify_queue.append(
        Answer(200, {"isValid": True, "payer": facilitator.payer}, delay=0.8)
    )
    response = harness.pay(1)
    assert response.status_code == 503 and response.json() == {"error": MSG_VERIFY}


def test_definite_verify_refusals_stay_402(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make()
    harness.offer()
    facilitator.verify_queue.append(
        Answer(200, {"isValid": False, "invalidReason": "invalid_exact_canton_expired"})
    )
    refused = harness.pay(1)
    assert refused.status_code == 402 and "payment-required" in refused.headers
    facilitator.verify_queue.append(Answer(400, {"error": "malformed_payload"}))
    bad_shape = harness.pay(2)
    assert bad_shape.status_code == 402
    assert harness.runs == 0 and facilitator.count("/settle") == 0


def test_verify_outage_clears_after_recovery(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make()
    harness.offer()
    facilitator.verify_queue.append(Answer(503, {"error": "scan_busy"}))
    assert harness.pay(1).status_code == 503
    assert harness.pay(1).status_code == 200  # the same payment, retried


def test_a_bare_client_error_after_an_unknown_call_is_still_unknown(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make()
    facilitator.settle_queue.extend([pending(), bare(400)])
    assert harness.pay(1).status_code == 200  # retried through the bare 400 to the success
    assert facilitator.count("/settle") == 3


def test_a_parsed_verdict_after_an_unknown_call_is_definite(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make()
    facilitator.settle_queue.extend([pending(), refuse("invalid_exact_canton_expired")])
    response = harness.pay(1)
    assert response.status_code == 402
    assert facilitator.count("/settle") == 2


def test_legacy_answers_a_repeat_with_success_and_is_still_delivered_once(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    facilitator.legacy = True
    harness = make()
    facilitator.settle_queue.append(pending(commit=True))
    assert (
        harness.pay(1).status_code == 200
    )  # the repeat is a success carrying the original updateId
    assert harness.pay(1).status_code == 402
