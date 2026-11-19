"""The facilitator client on its own, over a scripted transport: verdict classification and
the steps of settle that the end-to-end tests only see from outside."""

from __future__ import annotations

import logging
from collections import deque
from collections.abc import Callable
from typing import Any, cast

import pytest
from x402.schemas import PaymentPayload, PaymentRequirements

from ftptech_x402_canton.facilitator import (
    CantonFacilitator,
    FacilitatorSettings,
    FacilitatorUnavailableError,
    MissingStateError,
    VerifyRefusedError,
    _Kind,
    classify_settle,
)
from ftptech_x402_canton.identity import identity_from_payload, payment_digest
from ftptech_x402_canton.ledger import Ledger
from ftptech_x402_canton.state import CURRENT_STATE, RequestState
from ftptech_x402_canton.store import STATE_ATTEMPT, STATE_SETTLED, Record
from ftptech_x402_canton.transport import HttpAnswer, Transport
from tests.stores import SpyStore

H = "ab" * 32
ACCEPTED: dict[str, Any] = {
    "scheme": "exact", "network": "canton:mainnet", "asset": "CC", "amount": "100000000",
    "payTo": "merchant::1220" + "bb" * 32, "maxTimeoutSeconds": 120, "extra": {},
}  # fmt: skip
OK = {"success": True, "transaction": "1220uX", "network": "canton:mainnet", "payer": "p::1"}


def answer(
    status: int = 200, body: Any = None, retry_after: float = 0.0, error: str = ""
) -> HttpAnswer:
    return HttpAnswer(status, body, retry_after, error)


@pytest.mark.parametrize(
    ("given", "kind"),
    [
        (answer(200, OK), _Kind.SUCCESS),
        (answer(200, {**OK, "transaction": ""}), _Kind.UNKNOWN),
        (answer(202, OK), _Kind.UNKNOWN),  # a success that is not a 200 is not a verdict
        (answer(200, {"success": True}), _Kind.UNKNOWN),
        (answer(200, {"success": False, "errorReason": "settlement_pending", "transaction": "h"}), _Kind.UNKNOWN),
        (answer(503, {"success": False, "errorReason": "settlement_pending", "transaction": "h"}), _Kind.UNKNOWN),
        (answer(200, {"success": False, "errorReason": "invalid_exact_canton_payment_already_settled", "transaction": "u"}), _Kind.ALREADY_SETTLED),
        (answer(200, {"success": False, "errorReason": "invalid_exact_canton_payment_already_settled", "transaction": ""}), _Kind.UNKNOWN),
        (answer(200, {"success": False, "errorReason": "invalid_exact_canton_expired"}), _Kind.REFUSED),
        (answer(400, {"success": False, "errorReason": "invalid_exact_canton_malformed_payload"}), _Kind.REFUSED),
        (answer(200, {"success": False}), _Kind.UNKNOWN),
        (answer(200, {"success": False, "errorReason": ""}), _Kind.UNKNOWN),
        (answer(200, {"success": "yes", "errorReason": "x"}), _Kind.UNKNOWN),
        (answer(200, {"errorReason": "x"}), _Kind.UNKNOWN),
        (answer(429, {"error": "rate_limited"}), _Kind.UNKNOWN),
        (answer(500, None), _Kind.UNKNOWN),
        (answer(502, "text"), _Kind.UNKNOWN),
        (answer(200, [1]), _Kind.UNKNOWN),
        (answer(0, None, error="ReadTimeout"), _Kind.UNKNOWN),
    ],
)  # fmt: skip
def test_classify_settle(given: HttpAnswer, kind: _Kind) -> None:
    assert classify_settle(given).kind is kind


class FakeTransport:
    """Answers scripted per path; a hook runs after each answer is chosen."""

    def __init__(
        self, settle: list[HttpAnswer] | None = None, verify: list[HttpAnswer] | None = None
    ) -> None:
        self.script = {"/settle": deque(settle or []), "/verify": deque(verify or [])}
        self.posts: list[str] = []
        self.after_post: Callable[[str], None] | None = None

    def post(self, path: str, body: bytes, timeout: float) -> HttpAnswer:
        self.posts.append(path)
        given = self.script[path].popleft()
        if self.after_post is not None:
            self.after_post(path)
        return given

    def get(self, path: str, timeout: float) -> HttpAnswer:
        raise AssertionError("not used")


def make(
    transport: FakeTransport,
    gone: list[bool] | None = None,
    prior: Record | None = None,
    store: SpyStore | None = None,
) -> tuple[CantonFacilitator, RequestState, SpyStore, PaymentPayload, PaymentRequirements]:
    spy = store or SpyStore()
    payload = PaymentPayload.model_validate(
        {"x402Version": 2, "accepted": ACCEPTED, "payload": {"preparedTxHash": H, "signature": "s"}}
    )
    requirements = PaymentRequirements.model_validate(ACCEPTED)
    identity = identity_from_payload(payload)
    assert identity is not None
    flag = gone if gone is not None else [False]
    state = RequestState(identity, "token-1", 100.0, lambda: flag[0], prior)
    CURRENT_STATE.set(
        state
    )  # the request whose SDK call is running (set around each call by the gate)
    settings = FacilitatorSettings(settle_calls=3, backoff=(0.0,), sleep=lambda s: None)
    facilitator = CantonFacilitator(
        cast(Transport, transport),  # the scripted double has the two methods the client uses
        Ledger(spy, logging.getLogger("unit")),
        settings,
        logging.getLogger("unit"),
    )
    return facilitator, state, spy, payload, requirements


def test_a_payer_who_left_after_settle_takes_no_claim_but_the_payment_is_recorded() -> None:
    gone = [False]
    transport = FakeTransport(settle=[answer(200, OK)])
    transport.after_post = lambda path: gone.__setitem__(
        0, True
    )  # the payer leaves while /settle runs
    facilitator, state, spy, payload, requirements = make(transport, gone)
    result = facilitator.settle(payload, requirements)
    assert result.success is False and result.transaction == "1220uX"
    assert state.withheld and state.withheld_tx == "1220uX"
    assert spy.inner.claimant("1220uX") is None  # no claim: nothing was delivered
    record = spy.inner.record(H)
    assert (
        record is not None and record.state == STATE_SETTLED and record.update_id == "1220uX"
    )  # so the retry is served


def test_a_payer_who_left_before_settle_is_not_charged() -> None:
    transport = FakeTransport(settle=[answer(200, OK)])
    facilitator, state, spy, payload, requirements = make(transport, [True])
    result = facilitator.settle(payload, requirements)
    assert result.success is False and transport.posts == []
    assert state.not_charged and spy.calls == []


def test_the_sdks_second_settle_call_after_an_unknown_outcome_touches_nothing() -> None:
    transport = FakeTransport(
        settle=[
            answer(503, {"success": False, "errorReason": "settlement_pending", "transaction": "h"})
        ]
        * 3
    )
    facilitator, state, spy, payload, requirements = make(transport)
    first = facilitator.settle(payload, requirements)
    assert first.error_reason == "settlement_pending" and transport.posts == ["/settle"] * 3
    seen_store_calls = len(spy.calls)
    second = facilitator.settle(payload, requirements)  # the SDK's own immediate retry
    assert second.error_reason == "settlement_pending" and second.transaction == first.transaction
    assert transport.posts == ["/settle"] * 3  # no fourth call
    assert len(spy.calls) == seen_store_calls  # and no store work


def test_an_unverified_payers_identity_is_never_recorded() -> None:
    transport = FakeTransport(
        verify=[answer(200, {"isValid": False, "invalidReason": "x", "payer": "forged::1"})]
    )
    facilitator, state, spy, payload, requirements = make(transport)
    result = facilitator.verify(payload, requirements)
    assert result.is_valid is False
    assert state.payer == "" and state.verified is False


def test_a_valid_verify_records_the_payer_and_marks_verified() -> None:
    transport = FakeTransport(verify=[answer(200, {"isValid": True, "payer": "agent::1"})])
    facilitator, state, spy, payload, requirements = make(transport)
    assert facilitator.verify(payload, requirements).is_valid
    assert state.payer == "agent::1" and state.verified


def test_verify_is_skipped_only_for_the_recorded_bytes_and_uses_the_recorded_payer() -> None:
    transport = FakeTransport()
    payload = PaymentPayload.model_validate(
        {"x402Version": 2, "accepted": ACCEPTED, "payload": {"preparedTxHash": H, "signature": "s"}}
    )
    requirements = PaymentRequirements.model_validate(ACCEPTED)
    prior = Record(STATE_ATTEMPT, payment_digest(payload, requirements), payer="recorded::1")
    facilitator, state, spy, payload, requirements = make(transport, prior=prior)
    result = facilitator.verify(payload, requirements)
    assert result.is_valid and result.payer == "recorded::1" and transport.posts == []
    assert state.verified and state.payer == "recorded::1"


@pytest.mark.parametrize(
    ("given", "unavailable"),
    [
        (answer(429, {"error": "r"}), True),
        (answer(503, {"error": "scan_busy"}), True),
        (answer(500, None), True),
        (answer(302, None), True),
        (answer(200, None), True),
        (answer(200, {"isValid": "yes"}), True),
        (answer(0, None, error="ConnectError"), True),
        (answer(400, {"error": "bad"}), False),
        (answer(404, None), False),
        (answer(422, {"isValid": False, "invalidReason": "r"}), False),
    ],
)
def test_verify_failure_classes(given: HttpAnswer, unavailable: bool) -> None:
    facilitator, state, spy, payload, requirements = make(FakeTransport(verify=[given]))
    expected = FacilitatorUnavailableError if unavailable else VerifyRefusedError
    if given.status == 422:
        assert facilitator.verify(payload, requirements).is_valid is False
        return
    with pytest.raises(expected):
        facilitator.verify(payload, requirements)
    assert state.verify_unavailable is unavailable


def test_the_client_refuses_a_payment_no_paid_request_holds() -> None:
    facilitator, state, spy, payload, requirements = make(FakeTransport())
    other = PaymentPayload.model_validate(
        {"x402Version": 2, "accepted": ACCEPTED, "payload": {"preparedTxHash": "cd" * 32}}
    )
    with pytest.raises(MissingStateError):
        facilitator.settle(other, requirements)
    with pytest.raises(MissingStateError):
        facilitator.verify(other, requirements)
    nameless = PaymentPayload.model_validate(
        {"x402Version": 2, "accepted": ACCEPTED, "payload": {"x": 1}}
    )
    with pytest.raises(MissingStateError):
        facilitator.settle(nameless, requirements)


def test_a_conflicting_settled_record_withholds_instead_of_delivering() -> None:
    transport = FakeTransport(settle=[answer(200, OK)])
    spy = SpyStore()
    spy.inner.put_record(H, Record(STATE_SETTLED, "another-digest", "1220uOTHER", "p"), 3600)
    facilitator, state, spy, payload, requirements = make(transport, store=spy)
    result = facilitator.settle(payload, requirements)
    assert result.success is False and state.withheld
    assert spy.inner.claimant("1220uX") is None
