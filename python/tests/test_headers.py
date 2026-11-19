"""Rules 12, 13, 16 and the browser page: what a gate answer carries and never carries."""

from __future__ import annotations

import base64
import json
import logging
from collections.abc import Callable

import httpx
import pytest

from tests.mock_facilitator import Answer, MockFacilitator, pending, refuse
from tests.stores import SpyStore
from tests.support import Harness, Out, tx_hash

ALLOWED = {
    "content-type", "cache-control", "retry-after", "payment-response", "payment-required", "content-length",
    "date", "server", "connection", "allow",
}  # fmt: skip


def leaky(harness: Harness) -> None:
    harness.handler = lambda seen: Out(
        200,
        b'{"data":1}',
        {"Content-Type": "application/json", "X-Leak": "secret", "Content-Length": "9999"},
        cookies=True,
    )


def assert_only_gate_headers(response: httpx.Response) -> None:
    names = {name.lower() for name in response.headers}
    assert "set-cookie" not in names and "x-leak" not in names, names
    assert names <= ALLOWED, names - ALLOWED


def test_gate_errors_carry_no_handler_headers(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    spy = SpyStore()
    harness = make(store=spy)
    leaky(harness)
    cases: list[tuple[str, int]] = []

    facilitator.settle_always = pending()
    cases.append(("unknown outcome", harness.pay(1).status_code))
    facilitator.settle_always = refuse("invalid_exact_canton_expired")
    cases.append(("refusal", harness.pay(2).status_code))
    facilitator.settle_always = Answer(
        200, {"success": True, "transaction": "", "network": "canton:mainnet"}
    )
    cases.append(("success without a transaction", harness.pay(3).status_code))
    facilitator.settle_always = None
    spy.inner.claim(facilitator.update_id(tx_hash(4)), "other", 3600)
    cases.append(("claimed by another request", harness.pay(4).status_code))
    spy.fail("claim", "raise", "raise", "raise")
    cases.append(("withheld", harness.pay(5).status_code))
    assert [code for _, code in cases] == [503, 402, 503, 503, 503]
    # Re-run each failure and inspect headers.
    leaky(harness)
    facilitator.settle_always = pending()
    assert_only_gate_headers(harness.pay(11))
    facilitator.settle_always = refuse("invalid_exact_canton_expired")
    assert_only_gate_headers(harness.pay(12))
    facilitator.settle_always = Answer(
        200, {"success": True, "transaction": "", "network": "canton:mainnet"}
    )
    assert_only_gate_headers(harness.pay(13))
    facilitator.settle_always = None
    spy.inner.claim(facilitator.update_id(tx_hash(14)), "other", 3600)
    assert_only_gate_headers(harness.pay(14))
    spy.fail("claim", "raise", "raise", "raise")
    assert_only_gate_headers(harness.pay(15))
    assert_only_gate_headers(harness.pay(14))  # a replay


def test_redirect_answer_carries_no_handler_headers(harness: Harness) -> None:
    harness.handler = lambda seen: Out(
        301, b"moved", {"Location": "/elsewhere", "X-Leak": "1"}, cookies=True
    )
    response = harness.pay(1)
    assert response.status_code == 502
    assert_only_gate_headers(response)


def test_gate_error_before_the_handler_keeps_only_gate_headers(
    harness: Harness, facilitator: MockFacilitator
) -> None:
    from tests.support import payment_header

    harness.offer()
    facilitator.verify_queue.append(Answer(503, {"error": "scan_busy"}))
    response = harness.client.get(
        "/paid", headers={"PAYMENT-SIGNATURE": payment_header(harness.offer(), tx_hash(1))}
    )
    assert response.status_code == 503
    assert_only_gate_headers(response)


def test_gate_errors_are_no_store_and_503s_say_retry_after_3(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make()
    facilitator.settle_always = pending()
    unknown = harness.pay(1)
    assert unknown.headers["cache-control"] == "no-store" and unknown.headers["retry-after"] == "3"
    facilitator.settle_always = None
    assert harness.pay(2).status_code == 200
    redeemed = harness.pay(2)
    assert redeemed.status_code == 402 and redeemed.headers["cache-control"] == "no-store"
    assert "retry-after" not in redeemed.headers
    offer = harness.client.get("/paid")
    assert offer.status_code == 402 and offer.headers["cache-control"] == "no-store"
    assert "payment-required" in offer.headers


def test_paid_error_pass_through_keeps_the_handlers_own_cache_control(harness: Harness) -> None:
    harness.handler = lambda seen: Out(404, b"nope", {"Cache-Control": "max-age=5"})
    assert harness.pay(1).headers["cache-control"] == "max-age=5"  # not ours: unsettled, unchanged


def test_a_browser_gets_a_tiny_canton_page_not_the_evm_paywall(harness: Harness) -> None:
    response = harness.client.get(
        "/paid", headers={"Accept": "text/html,application/xhtml+xml", "User-Agent": "Mozilla/5.0"}
    )
    assert response.status_code == 402
    assert response.headers["content-type"].startswith("text/html")
    assert len(response.content) < 600
    text = response.text
    assert "Payment required: 0.01 CC" in text
    assert "<script" not in text and "USDC" not in text and "eip155" not in text
    offer = json.loads(base64.b64decode(response.headers["payment-required"]))
    assert (
        offer["accepts"][0]["network"] == "canton:mainnet"
    )  # the offer is in the header, as for every client
    assert response.headers["cache-control"] == "no-store"


def test_an_api_client_gets_json(harness: Harness) -> None:
    response = harness.client.get("/paid", headers={"Accept": "application/json"})
    assert response.status_code == 402 and response.headers["content-type"].startswith(
        "application/json"
    )


def test_nothing_secret_is_ever_logged(
    make: Callable[..., Harness], facilitator: MockFacilitator, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level(logging.DEBUG)
    spy = SpyStore()
    harness = make(store=spy)
    secret_signature, secret_tx = "SIGNATURE-" + "S" * 200, "PREPARED-TRANSACTION-" + "T" * 200
    offer = harness.offer()
    from tests.support import payment_header

    def header(n: int) -> str:
        return payment_header(
            offer, tx_hash(n), signature=secret_signature, preparedTransaction=secret_tx
        )

    def pending_forever() -> None:
        facilitator.settle_always = pending()

    def claim_fails() -> None:
        facilitator.settle_always = None
        spy.fail("claim", "raise", "raise", "raise")

    def verify_outage() -> None:
        facilitator.verify_queue.append(Answer(503, {"error": "scan_busy"}))

    def verify_refused() -> None:
        facilitator.verify_queue.append(Answer(400, {"error": "bad"}))

    for n, setup in enumerate(
        [lambda: None, pending_forever, claim_fails, verify_outage, verify_refused], start=1
    ):
        setup()
        harness.client.get("/paid", headers={"PAYMENT-SIGNATURE": header(n)})
    harness.client.get("/paid", headers={"PAYMENT-SIGNATURE": header(1)})  # a replay
    harness.client.get("/paid", headers={"PAYMENT-SIGNATURE": "junk"})
    text = "\n".join(
        record.getMessage() + " " + json.dumps(getattr(record, "canton", {}), default=str)
        for record in caplog.records
    )
    assert len(caplog.records) > 5
    for secret in (secret_signature, secret_tx, header(1), "PAYMENT-SIGNATURE", "junk"):
        assert secret not in text
    assert tx_hash(2) in text  # the canonical hash is what is logged, for reconciliation
