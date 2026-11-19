"""The facilitator client: the SDK's verify/settle protocol with the rules the SDK lacks.

The SDK's own HTTP client raises on any non-200 answer, so our 503
``settlement_pending`` never reaches its retry, and it has no notion of a store.
This client:

- parses non-200 bodies and retries the identical bytes on ``settlement_pending``,
  transport errors, timeouts, 429 and 5xx, within a budget counted in calls (the
  SDK's own retry included);
- treats any answer without a parsed verdict as unknown;
- records the attempt before the first /settle and the settled payment after it,
  and takes the delivery claim (one delivery per updateId);
- never ties /settle to the payer's socket: it runs to its own timeout in the
  calling thread.
"""

from __future__ import annotations

import logging
import time
from collections.abc import Callable, Sequence
from dataclasses import dataclass
from enum import Enum
from typing import Any

from pydantic import ValidationError
from x402.schemas import (
    PaymentPayload,
    PaymentRequirements,
    SettleResponse,
    SupportedResponse,
    VerifyResponse,
)

from .errors import StoreFullError
from .identity import (
    digest_matches,
    identity_from_payload,
    legacy_payment_digest,
    payment_digest,
    request_body,
)
from .ledger import Ledger
from .logs import event
from .mechanisms.canton import CANTON_MAINNET_CAIP2, ERR_SETTLEMENT_PENDING
from .responses import (
    ERR_REASON_ALREADY_REDEEMED,
    ERR_REASON_ALREADY_SETTLED,
    MSG_PAYER_LEFT,
    MSG_STORE_FULL,
)
from .state import CURRENT_STATE, RequestState
from .store import STATE_ATTEMPT, STATE_SETTLED, Record
from .transport import HttpAnswer, Transport

#: Pseudo reasons carried by a refusal that the gate turns into its own answer.
REASON_PAYER_LEFT = "payer_left"
REASON_STORE_FULL = "payment_store_full"
REASON_DELIVERY_WITHHELD = "payment_delivery_withheld"

_REASON_MAX_CHARS = 120
#: How long a definite /settle refusal proves that an attempt did not commit: the facilitator keeps its record for 24 hours.
ATTEMPT_TRUST_WINDOW_SECONDS = 23 * 3600


class FacilitatorUnavailableError(Exception):
    """/verify gave no usable answer (429, 5xx, timeout, garbage): nothing was settled."""


class VerifyRefusedError(Exception):
    """/verify refused the request with a 4xx: a definite answer, nothing was submitted."""


class MissingStateError(Exception):
    """The SDK called the client for a payment the gate does not hold: refuse, never settle blind."""


@dataclass(frozen=True)
class FacilitatorSettings:
    verify_timeout: float = 30.0
    settle_timeout: float = 65.0
    settle_calls: int = 4
    #: Longest a facilitator Retry-After may hold a paid request.
    retry_after_cap: float = 5.0
    #: The wait before settle retry 1, 2, 3 when the facilitator sends no
    #: Retry-After. Matches PENDING_RETRY_MS in the Express middleware.
    backoff: Sequence[float] = (1.0, 2.0, 4.0)
    sleep: Callable[[float], None] = time.sleep


class _Kind(Enum):
    SUCCESS = "success"
    ALREADY_SETTLED = "already_settled"
    REFUSED = "refused"
    UNKNOWN = "unknown"


@dataclass(frozen=True)
class _Verdict:
    kind: _Kind
    reason: str = ""
    transaction: str = ""
    payer: str = ""


def classify_settle(answer: HttpAnswer) -> _Verdict:
    """Only a parsed facilitator verdict is definite.

    Definite: ``success:true`` with a transaction (HTTP 200), or ``success:false``
    with an ``errorReason`` other than ``settlement_pending``. Everything else is
    unknown and is retried: ``settlement_pending`` in any form, a transport error or
    timeout, 429, and every other answer without an errorReason (a bare 4xx or 5xx
    from a proxy, and a ``{success:false}`` with no reason, included).
    """
    body = answer.body
    if not isinstance(body, dict):
        return _Verdict(_Kind.UNKNOWN)
    success = body.get("success")
    reason = body.get("errorReason")
    reason = reason if isinstance(reason, str) else ""
    transaction = body.get("transaction")
    transaction = transaction if isinstance(transaction, str) else ""
    payer = body.get("payer")
    payer = payer if isinstance(payer, str) else ""
    if success is True:
        if answer.status == 200 and transaction:
            return _Verdict(_Kind.SUCCESS, transaction=transaction, payer=payer)
        return _Verdict(_Kind.UNKNOWN, transaction=transaction, payer=payer)
    if success is False and reason:
        if reason == ERR_SETTLEMENT_PENDING:
            return _Verdict(_Kind.UNKNOWN, reason, transaction, payer)
        if reason == ERR_REASON_ALREADY_SETTLED:
            # Without the updateId there is nothing to deliver and nothing proven: the
            # payment moved. It is not a refusal, and the attempt must stay.
            kind = _Kind.ALREADY_SETTLED if transaction else _Kind.UNKNOWN
            return _Verdict(kind, reason, transaction, payer)
        return _Verdict(_Kind.REFUSED, reason, transaction, payer)
    return _Verdict(_Kind.UNKNOWN, transaction=transaction, payer=payer)


def _refusal(reason: str, transaction: str = "", payer: str = "") -> SettleResponse:
    return SettleResponse(
        success=False,
        error_reason=reason,
        transaction=transaction,
        network=CANTON_MAINNET_CAIP2,
        payer=payer or None,
    )


class CantonFacilitator:
    """Implements the SDK's sync facilitator protocol (``verify``, ``settle``, ``get_supported``)."""

    def __init__(
        self,
        transport: Transport,
        ledger: Ledger,
        settings: FacilitatorSettings,
        logger: logging.Logger,
    ) -> None:
        self._transport = transport
        self._ledger = ledger
        self._settings = settings
        self._log = logger
        self._primed: SupportedResponse | None = None

    # -- /supported ---------------------------------------------------------

    def fetch_supported(self, timeout: float) -> SupportedResponse:
        """One ``GET /supported`` with a timeout. Raises ``FacilitatorUnavailableError``."""
        answer = self._transport.get("/supported", timeout)
        if answer.status != 200 or not isinstance(answer.body, dict):
            reason = answer.error or f"HTTP {answer.status}"
            raise FacilitatorUnavailableError(f"GET /supported failed: {reason}")
        try:
            return SupportedResponse.model_validate(answer.body)
        except ValidationError as error:
            raise FacilitatorUnavailableError(
                "GET /supported answered an unreadable body"
            ) from error

    def prime(self, supported: SupportedResponse) -> None:
        """Hand the next ``get_supported`` call a response already fetched, so the
        SDK's own initialization does not read /supported a second time."""
        self._primed = supported

    def get_supported(self) -> SupportedResponse:
        primed, self._primed = self._primed, None
        if primed is not None:
            return primed
        return self.fetch_supported(self._settings.verify_timeout)

    # -- /verify ------------------------------------------------------------

    def verify(self, payload: PaymentPayload, requirements: PaymentRequirements) -> VerifyResponse:
        """Call /verify. A failure that is not a definite answer is flagged in the
        request state, so the gate answers 503 instead of the SDK's 402: a 402 tells
        the payer the payment was refused, and a client may then pay again."""
        state = self._state_for(payload)
        prior = state.prior
        if prior is not None and digest_matches(prior.digest, payload, requirements):
            # A payment this merchant already attempted or saw settle, with the very
            # bytes recorded. After the payment deadline /verify would say "expired".
            # /settle still decides: the facilitator answers a repeat from its record.
            with state.mutex:
                state.verified = True
                state.payer = prior.payer
            return VerifyResponse(is_valid=True, payer=prior.payer or None)
        self._begin_call(state, settle=False)
        answer = self._transport.post(
            "/verify", request_body(payload, requirements), self._settings.verify_timeout
        )
        return self._read_verify(state, answer)

    def _read_verify(self, state: RequestState, answer: HttpAnswer) -> VerifyResponse:
        definite_status = answer.status == 200 or (
            400 <= answer.status < 500 and answer.status != 429
        )
        body = answer.body
        if definite_status and isinstance(body, dict) and isinstance(body.get("isValid"), bool):
            try:
                result = VerifyResponse.model_validate(body)
            except ValidationError:
                result = None
            if result is not None:
                if result.is_valid:
                    # The payer of an isValid:false answer is never used: it can be a forged act_as.
                    with state.mutex:
                        state.verified = True
                        if result.payer:
                            state.payer = result.payer
                return result
        if 400 <= answer.status < 500 and answer.status != 429:
            raise VerifyRefusedError(
                f"facilitator refused /verify ({answer.status}){_reason_of(body)}"
            )
        with state.mutex:
            state.verify_unavailable = True
        event(
            self._log,
            logging.WARNING,
            "verify unavailable",
            status=answer.status,
            error=answer.error,
            canonicalHash=state.identity.canonical_hash,
        )  # METRICS: count verify outages here
        raise FacilitatorUnavailableError("verification unavailable")

    # -- /settle ------------------------------------------------------------

    def settle(self, payload: PaymentPayload, requirements: PaymentRequirements) -> SettleResponse:
        """Settle once per payment: record the attempt, call /settle with the identical
        bytes within the call budget, then record and claim the delivery."""
        state = self._state_for(payload)
        with state.mutex:
            already_unknown = state.settle_unknown
        if already_unknown:
            # The SDK's own immediate retry after our pending answer: the budget is
            # spent, so no call is made and the same unknown outcome comes back.
            return self._unknown(state)
        gone = self._refuse_if_payer_gone(state)
        if gone is not None:
            return gone
        try:
            return self._settle(state, payload, requirements)
        except Exception as error:
            # Never a 402 once a /settle call may have been made: the money may have moved.
            if self._calls(state) == 0 and state.prior is None:
                raise
            event(
                self._log,
                logging.ERROR,
                "unexpected error after a /settle call: outcome unknown",
                canonicalHash=state.identity.canonical_hash,
                errorType=type(error).__name__,
            )
            return self._unknown(state)

    def _settle(
        self, state: RequestState, payload: PaymentPayload, requirements: PaymentRequirements
    ) -> SettleResponse:
        digest = payment_digest(payload, requirements)
        prior = state.prior
        if (
            prior is not None
            and prior.state == STATE_SETTLED
            and prior.update_id
            and digest_matches(prior.digest, payload, requirements)
        ):
            # The gate holds a settled record for these very bytes: the payment
            # moved, so /settle is not asked (the facilitator may have forgotten
            # it). The recorded updateId is delivered once, after the claim.
            return self._deliver(
                state, prior.update_id, prior.payer or state.payer, digest, record_known=True
            )
        if not self._record_attempt(state, digest):
            return _refusal(REASON_STORE_FULL)
        digests = (digest, legacy_payment_digest(payload, requirements))
        return self._settle_with_retries(state, request_body(payload, requirements), digests)

    def _refuse_if_payer_gone(self, state: RequestState) -> SettleResponse | None:
        if not state.payer_gone():
            return None
        with state.mutex:
            state.not_charged, state.not_charged_message = True, MSG_PAYER_LEFT
        event(
            self._log,
            logging.WARNING,
            "payer left before settle: not settled",
            canonicalHash=state.identity.canonical_hash,
        )
        return _refusal(REASON_PAYER_LEFT)

    def _record_attempt(self, state: RequestState, digest: str) -> bool:
        """Record the attempt before the first /settle call, so a retry after an
        unknown outcome, a crash or a store failure can skip /verify. False when the
        memory store is full: nothing is settled."""
        attempt = Record(
            STATE_ATTEMPT, digest, payer=state.payer, at=int(self._ledger.wall_clock())
        )
        try:
            self._ledger.save_attempt(state.identity.canonical_hash, attempt)
        except StoreFullError:
            with state.mutex:
                state.not_charged, state.not_charged_message = True, MSG_STORE_FULL
            event(
                self._log,
                logging.ERROR,
                "memory store is full: payment not settled; raise max_entries or use RedisStore",
                canonicalHash=state.identity.canonical_hash,
            )
            return False
        except Exception as error:
            # Settle goes on: the attempt is kept in process, and the payer must not be refused for our store.
            event(
                self._log,
                logging.ERROR,
                "attempt not confirmed in the store: kept in process",
                canonicalHash=state.identity.canonical_hash,
                error=str(error),
            )
        return True

    def _settle_with_retries(
        self, state: RequestState, body: bytes, digests: tuple[str, str]
    ) -> SettleResponse:
        retry = 0
        while True:
            if self._calls(state) >= self._settings.settle_calls:
                return self._unknown(state)
            self._begin_call(state, settle=True)
            # METRICS: track /settle latency per call here
            answer = self._transport.post("/settle", body, self._settings.settle_timeout)
            verdict = classify_settle(answer)
            if verdict.kind is not _Kind.UNKNOWN:
                return self._act_on(state, verdict, digests)
            self._note_unknown(state, verdict, answer)
            if self._calls(state) >= self._settings.settle_calls:
                return self._unknown(state)
            self._settings.sleep(self._delay(state, retry))
            retry += 1

    def _act_on(
        self, state: RequestState, verdict: _Verdict, digests: tuple[str, str]
    ) -> SettleResponse:
        if verdict.kind in (_Kind.SUCCESS, _Kind.ALREADY_SETTLED):
            # SUCCESS is the settle itself; ALREADY_SETTLED is the facilitator's answer
            # to a repeat, with the original updateId. Either way: one delivery.
            return self._deliver(
                state,
                verdict.transaction,
                verdict.payer or state.payer,
                digests[0],
                record_known=False,
            )
        # A definite refusal: nothing moved, so a retry goes through /verify again.
        hash = state.identity.canonical_hash
        try:
            record = self._ledger.lookup(hash)
        except Exception:
            record = None
        # The oldest attempt known for these bytes: a store blip must not make an old one look new.
        ages = [
            r.at
            for r in (state.prior, record)
            if r is not None and r.state == STATE_ATTEMPT and r.at and r.digest in digests
        ]
        if ages and self._ledger.wall_clock() - min(ages) > ATTEMPT_TRUST_WINDOW_SECONDS:
            # The facilitator's own record has lapsed: a refusal no longer proves that the
            # first send did not commit. The outcome stays unknown and the attempt stays.
            event(
                self._log,
                logging.WARNING,
                "refusal for an attempt older than the facilitator's record: outcome stays unknown",
                canonicalHash=hash,
            )
            return self._unknown(state)
        prior = state.prior
        if prior is not None and not (prior.state == STATE_ATTEMPT and prior.digest in digests):
            # The payment is recorded as settled, or as attempted with other bytes: a refusal of
            # these bytes proves nothing about it. The money may have moved. Decided before any
            # clear, so this request never deletes that record.
            return self._unknown(state)
        if record is not None and record.state == STATE_ATTEMPT and record.digest in digests:
            self._ledger.clear(hash)
        with state.mutex:
            state.refused = True
        return _refusal(verdict.reason, "", verdict.payer)

    def _deliver(
        self, state: RequestState, update_id: str, payer: str, digest: str, *, record_known: bool
    ) -> SettleResponse:
        """Record the settled payment and take the delivery claim.

        Nothing is delivered without a claim: a store failure, or a payer who looks
        gone, withholds the body and answers 503; the settled record lets the retry
        be served once.
        """
        if not record_known:
            record = Record(STATE_SETTLED, digest, update_id, payer)
            try:
                self._ledger.save_settled(state.identity.canonical_hash, record)
            except Exception as error:
                return self._withhold(state, update_id, error)
        if state.payer_gone():
            return self._withhold(state, update_id, None)
        try:
            claimed = self._ledger.claim(update_id, state.token)
        except Exception as error:
            return self._withhold(state, update_id, error)
        if not claimed:
            with state.mutex:
                state.redeemed, state.redeemed_tx = True, update_id
            return _refusal(ERR_REASON_ALREADY_REDEEMED, update_id, payer)
        with state.mutex:
            state.claimed_tx = update_id
        return SettleResponse(
            success=True,
            transaction=update_id,
            network=CANTON_MAINNET_CAIP2,
            payer=payer or None,
        )

    def _withhold(
        self, state: RequestState, update_id: str, cause: Exception | None
    ) -> SettleResponse:
        """Keep the body back after money moved. The gate answers 503 with the
        updateId; the settled record lets a retry of the same payment be served once."""
        with state.mutex:
            state.withheld, state.withheld_tx = True, update_id
        event(
            self._log,
            logging.ERROR if cause is not None else logging.WARNING,
            "settled but not delivered: a retry of the same payment is served once; "
            "reconcile by updateId",
            canonicalHash=state.identity.canonical_hash,
            updateId=update_id,
            cause=str(cause) if cause is not None else "payer left",
        )
        return _refusal(REASON_DELIVERY_WITHHELD, update_id)

    def _unknown(self, state: RequestState) -> SettleResponse:
        """The budget is spent without a definite answer. Reads as settlement_pending
        to the gate, which turns it into a 503, never a 402."""
        with state.mutex:
            already_logged = state.settle_unknown
            state.settle_unknown = True
            transaction = state.identity.prepared_tx_hash or state.last_tx
            payer, calls = state.last_payer, state.settle_calls
        if not already_logged:  # the SDK's own retry arrives here too: log once
            event(
                self._log,
                logging.WARNING,
                "settle outcome unknown: do not pay again, retry the same payment",
                canonicalHash=state.identity.canonical_hash,
                settleCalls=calls,
            )  # METRICS: count unknown outcomes here
        return _refusal(ERR_SETTLEMENT_PENDING, transaction, payer)

    # -- helpers --------------------------------------------------------------

    @staticmethod
    def _state_for(payload: PaymentPayload) -> RequestState:
        """The state of the request this SDK call runs for, checked against the payload."""
        state = CURRENT_STATE.get()
        identity = identity_from_payload(payload)
        if (
            state is None
            or identity is None
            or identity.canonical_hash != state.identity.canonical_hash
        ):
            raise MissingStateError("payment is not held by a paid request: refused")
        return state

    @staticmethod
    def _calls(state: RequestState) -> int:
        with state.mutex:
            return state.settle_calls

    @staticmethod
    def _begin_call(state: RequestState, *, settle: bool) -> None:
        """Clear the answer of the previous call and count the call when it is a settle."""
        with state.mutex:
            state.last_status, state.last_retry_after = 0, 0.0
            if settle:
                state.settle_calls += 1

    @staticmethod
    def _note_unknown(state: RequestState, verdict: _Verdict, answer: HttpAnswer) -> None:
        with state.mutex:
            state.last_status, state.last_retry_after = answer.status, answer.retry_after
            if verdict.transaction:
                state.last_tx = verdict.transaction
            if verdict.payer:
                state.last_payer = verdict.payer

    def _delay(self, state: RequestState, retry: int) -> float:
        backoff = self._settings.backoff
        delay = backoff[min(retry, len(backoff) - 1)] if backoff else 0.0
        delay = min(
            delay, self._settings.retry_after_cap
        )  # the pauses are counted in the budget at the cap
        with state.mutex:
            retry_after = state.last_retry_after
        if retry_after > 0:
            delay = min(retry_after, self._settings.retry_after_cap)
        return delay


def _reason_of(body: Any) -> str:
    if isinstance(body, dict):
        for key in ("invalidReason", "error", "errorReason"):
            value = body.get(key)
            if isinstance(value, str) and value:
                return ": " + value[:_REASON_MAX_CHARS]
    return ""
