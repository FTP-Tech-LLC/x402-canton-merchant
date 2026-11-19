"""The sync core of a paid request: every money decision lives here, drivers only do I/O.

The ASGI and WSGI drivers call, in order: ``Gate.begin`` (route, payment header,
delivered check, in-flight lock), ``PaidRequest.verify`` (SDK request processing:
the 402 offer, matching, /verify), the paid handler (the driver runs it and
captures its answer), ``PaidRequest.finish`` (status policy, /settle, delivery
claim) and, always, ``PaidRequest.close`` (release the lock).
"""

from __future__ import annotations

import json
import logging
import threading
from collections.abc import Callable
from dataclasses import dataclass
from enum import Enum

from x402.http.constants import PAYMENT_SIGNATURE_HEADER
from x402.http.types import (
    HTTPProcessResult,
    HTTPRequestContext,
    HTTPResponseInstructions,
    HTTPTransportContext,
    ProcessSettleResult,
)
from x402.http.x402_http_server import x402HTTPResourceServerSync

from . import forksafe
from .config import POST_VERIFY_STORE_CALLS, Resolved, loose_key, store_call_budget
from .errors import StoreFullError
from .facilitator import MissingStateError
from .identity import HeaderOutcome, new_token, parse_payment_header
from .ledger import ClaimState, Ledger
from .logs import event
from .responses import (
    HEADER_CACHE_CONTROL,
    HEADER_CONTENT_LENGTH,
    HEADER_TRANSFER_ENCODING,
    MSG_HEAD_WITH_PAYMENT,
    MSG_INTERNAL,
    MSG_INVALID_STATUS,
    MSG_NO_HASH,
    MSG_NOT_VERIFIED,
    MSG_PAYMENT_IN_PROGRESS,
    MSG_REDIRECT_NOT_CHARGED,
    MSG_SETTLEMENT_FAILED,
    MSG_STORE_FULL,
    MSG_STORE_UNAVAILABLE,
    MSG_VERIFY_UNAVAILABLE,
    RESERVED_HANDLER_HEADERS,
    GateResponse,
    Headers,
    already_redeemed,
    gate_error,
    outcome_unknown,
    with_private_cache_control,
)
from .state import CURRENT_STATE, RequestState, StateRegistry
from .store import STATE_SETTLED

STATUS_NO_CONTENT, STATUS_NOT_MODIFIED = 204, 304
MAX_HTTP_STATUS = 999


class RouteDecision(Enum):
    FREE = "free"  # not a paid route: pass through untouched
    REFUSE = "refuse"  # a spelling the SDK does not gate but a loose router might serve as the paid route
    PAID = "paid"
    NOT_READY = "not_ready"  # a paid route, but startup has not finished


@dataclass
class HandlerResponse:
    """What the paid handler answered, buffered whole."""

    status: int
    headers: Headers
    body: bytes


class Gate:
    """Per-server factory of :class:`PaidRequest`. One instance, shared by all threads."""

    def __init__(
        self,
        resolved: Resolved,
        ledger: Ledger,
        registry: StateRegistry,
        lifetimes: dict[str, int],
    ) -> None:
        self._resolved = resolved
        self._ledger = ledger
        self._registry = registry
        self._lifetimes = lifetimes  # loose route key -> executeBeforeSeconds
        self._log = resolved.logger
        self.http_server: x402HTTPResourceServerSync | None = None
        #: Set when the server begins to close: a request with a payment is then refused, so
        #: nothing starts a paid flow that no reconciler will heal.
        self.closing = False
        self._not_ready_logged = False
        self._admit_mutex = threading.Lock()
        #: Called once per paid request (with a payment): starts what a fork does not carry over.
        self.on_paid_request: Callable[[], None] | None = None
        forksafe.register(self)

    def reset_after_fork(self) -> None:
        self._admit_mutex = threading.Lock()

    # -- routing ------------------------------------------------------------

    def _loose_match(self, ctx: HTTPRequestContext, alt_paths: tuple[str, ...]) -> str | None:
        """The loose route key the request looks like, over every spelling of its path.

        ``alt_paths`` are other views of the path (a mounted app's full path, with its
        mount prefix): a key may be written in either form.
        """
        method = ctx.method.upper()
        for path in (ctx.decoded_path or ctx.path.split("?")[0], *alt_paths):
            loose = loose_key(method, path)
            if loose in self._lifetimes:
                return loose
        return None

    def decide(self, ctx: HTTPRequestContext, alt_paths: tuple[str, ...] = ()) -> RouteDecision:
        looks_paid = self._loose_match(ctx, alt_paths) is not None
        server = self.http_server
        if server is None:
            return RouteDecision.NOT_READY if looks_paid else RouteDecision.FREE
        if server.requires_payment(ctx):
            return RouteDecision.PAID
        return RouteDecision.REFUSE if looks_paid else RouteDecision.FREE

    def budget_for(self, ctx: HTTPRequestContext, alt_paths: tuple[str, ...] = ()) -> float:
        """The longest a paid request to this route may take: the larger of ``timeout``
        and the payment's own lifetime, then every settle call, the pauses between
        them, and the store calls after /verify. The in-flight lock uses it."""
        config = self._resolved.config
        matched = self._loose_match(ctx, alt_paths)
        lifetime = float(
            self._lifetimes[matched] if matched is not None else max(self._lifetimes.values())
        )
        return (
            max(self._resolved.timeout, lifetime)
            + config.settle_calls * config.settle_timeout
            + max(config.settle_calls - 1, 0) * config.retry_after_cap
            + POST_VERIFY_STORE_CALLS * store_call_budget(config.store)
        )

    # -- admission ----------------------------------------------------------

    def begin(
        self,
        ctx: HTTPRequestContext,
        payer_gone: Callable[[], bool],
        alt_paths: tuple[str, ...] = (),
    ) -> PaidRequest | GateResponse:
        """Apply the one-delivery rules before /verify: a delivered payment is
        refused, a payment in flight is told to wait, and otherwise the in-flight
        lock is taken. Returns the answer to send, or the request to go on with."""
        header = ctx.adapter.get_header(PAYMENT_SIGNATURE_HEADER) or ctx.adapter.get_header(
            PAYMENT_SIGNATURE_HEADER.lower()
        )
        if not header:
            return PaidRequest(self, ctx, None)
        if ctx.method.upper() == "HEAD":
            # A HEAD cannot carry the paid response, so it must not spend the payment.
            return gate_error(405, MSG_HEAD_WITH_PAYMENT, [("Allow", "GET")])
        if self.closing:
            return gate_error(503, MSG_PAYMENT_IN_PROGRESS)
        outcome, identity = parse_payment_header(header)
        if outcome is HeaderOutcome.NO_HASH:
            return gate_error(400, MSG_NO_HASH)
        if outcome is HeaderOutcome.UNREADABLE or identity is None:
            return PaidRequest(self, ctx, None)  # the SDK answers a header it cannot read
        if self.on_paid_request is not None:
            self.on_paid_request()
        state = RequestState(identity, new_token(), self.budget_for(ctx, alt_paths), payer_gone)
        answered = self._guard(state)
        if answered is not None:
            return answered
        with (
            self._admit_mutex
        ):  # closing is set under the same mutex: registered before it, or refused
            refused = self.closing
            if not refused:
                self._registry.add(state)
        if refused:
            self._ledger.unlock(state.identity.canonical_hash, state.token, state.budget)
            return gate_error(503, MSG_PAYMENT_IN_PROGRESS)
        return PaidRequest(self, ctx, state)

    def note_not_ready(self) -> None:
        """Log once when a paid route is hit before startup finished (a server that never started)."""
        if not self._not_ready_logged:
            self._not_ready_logged = True
            event(
                self._log,
                logging.ERROR,
                "a paid route was requested before CantonServer.start() finished: it answers 503 until "
                "then. Start the server in the top-level app's lifespan or at import",
            )

    def reopen(self) -> None:
        with self._admit_mutex:
            self.closing = False

    def begin_closing(self) -> None:
        with self._admit_mutex:
            self.closing = True

    def _guard(self, state: RequestState) -> GateResponse | None:
        digest_hash = state.identity.canonical_hash
        answer = self._answer_if_delivered_or_failed(state)
        if answer is not None:
            return answer
        try:
            acquired = self._ledger.lock(digest_hash, state.token, state.budget)
        except Exception as error:
            self._log_store_error("lock", digest_hash, error)
            return self._store_unavailable(state, error)
        if not acquired:
            return gate_error(503, MSG_PAYMENT_IN_PROGRESS)
        # A request that finished between the first read and the lock may have
        # recorded or delivered the payment: look again under the lock.
        answer = self._answer_if_delivered_or_failed(state)
        if answer is not None:
            self._ledger.unlock(digest_hash, state.token, state.budget)
        return answer

    def _answer_if_delivered_or_failed(self, state: RequestState) -> GateResponse | None:
        """Answer 402 for a delivered payment and 503 for a store failure. A payment
        that was attempted or settled but not delivered is noted in the state, so
        /verify can be skipped for the very same bytes."""
        digest_hash = state.identity.canonical_hash
        try:
            record = self._ledger.lookup(digest_hash)
            state.prior = record  # known before the claim is read: a failure there names it
            if record is not None and record.state == STATE_SETTLED and record.update_id:
                ownership = self._ledger.claim_state(record.update_id)
                if ownership is ClaimState.DELIVERED:
                    return already_redeemed(record.update_id)
                if ownership is ClaimState.HELD:
                    # Claimed by a request that has not confirmed its delivery: not the final
                    # 402. The payer retries; a delivered claim is marked, a dead one is not.
                    return outcome_unknown(record.update_id)
        except Exception as error:
            self._log_store_error("read", digest_hash, error)
            return self._store_unavailable(state, error)
        return None

    def _store_unavailable(self, state: RequestState, error: Exception) -> GateResponse:
        """A store failure before /verify. A full memory store means nothing was
        charged. Otherwise the gate cannot know the state of the payment, so it never
        says "nothing was settled": it tells the payer not to pay again, and names the
        updateId when it already holds a settled record."""
        if state.prior is not None:  # a recorded payment may have moved, whatever the store says
            return outcome_unknown(state.prior.update_id or state.identity.prepared_tx_hash)
        if isinstance(error, StoreFullError):
            return gate_error(503, MSG_STORE_FULL)
        return gate_error(503, MSG_STORE_UNAVAILABLE)

    def _log_store_error(self, operation: str, digest_hash: str, error: Exception) -> None:
        event(
            self._log,
            logging.ERROR,
            "store call failed",
            operation=operation,
            canonicalHash=digest_hash,
            error=str(error),
        )  # METRICS: count store failures here

    def release(self, state: RequestState) -> None:
        # Unlock first, then leave the registry: close() waits for the registry, so a release in
        # flight is waited for too.
        self._ledger.unlock(state.identity.canonical_hash, state.token, state.budget)
        self._registry.remove(state)

    def log_context(self) -> logging.Logger:
        return self._log

    @property
    def ledger(self) -> Ledger:
        return self._ledger

    @property
    def settle_on_redirect(self) -> bool:
        return self._resolved.config.settle_on_redirect


class PaidRequest:
    """One request on a paid route. ``state`` is None for an unpaid request."""

    def __init__(self, gate: Gate, ctx: HTTPRequestContext, state: RequestState | None) -> None:
        self._gate = gate
        self._ctx = ctx
        self.state = state
        self._result: HTTPProcessResult | None = None
        self._closed = False
        self._close_mutex = threading.Lock()

    @property
    def payer(self) -> str:
        """The payer /verify proved, or recorded for a retry. Empty when none is known."""
        return self.state.payer if self.state is not None else ""

    @property
    def is_retry(self) -> bool:
        """True for the retry of a payment this merchant already attempted or settled but did not deliver."""
        return self.state is not None and self.state.prior is not None

    def close(self) -> None:
        """Release the in-flight lock. Idempotent; call it when the request ends."""
        with self._close_mutex:
            if self._closed:
                return
            self._closed = True
        if self.state is not None:
            self._gate.release(self.state)

    def will_settle(self, handler: HandlerResponse) -> bool:
        """Whether this answer will be settled: only then does the gate wait for the rest of the
        request body (a refusal is answered at once)."""
        status = handler.status
        return 200 <= status < 300 or (300 <= status < 400 and self._gate.settle_on_redirect)

    def finish_delivery(self, delivered: bool) -> None:
        """After the response was written: mark the claim delivered, then release the lock.

        Only a claim marked delivered is answered as already delivered (the final 402).
        If the mark fails the claim stays unmarked and replays answer 503 until it lapses.
        """
        state = self.state
        if delivered and state is not None:
            with state.mutex:
                update_id = state.claimed_tx
            if update_id:
                self._gate.ledger.mark_delivered(update_id, state.token)
        self.close()

    # -- verify -------------------------------------------------------------

    def verify(self) -> GateResponse | None:
        """Run the SDK's request processing. Returns the answer to send (the 402 offer,
        a refusal, a /verify outage as 503), or None when the payment is verified and
        the handler may run."""
        server = self._gate.http_server
        if server is None:
            return gate_error(503, MSG_INTERNAL)
        current = CURRENT_STATE.set(self.state)
        try:
            result = server.process_http_request(self._ctx, None)
        except Exception as error:
            return self._internal_error("verify", error, charged=False)
        finally:
            CURRENT_STATE.reset(current)
        if result.type == "payment-error":
            return self._payment_error(result.response)
        if (
            result.type != "payment-verified"
            or self.state is None
            or not self.state.verified
            or result.before_handler_settlement
            is not None  # an upfront flow settled before the handler
        ):
            # Fail closed: the handler runs only for a payment /verify passed (or one
            # this merchant recorded). If the SDK ever says otherwise, nothing is delivered.
            event(
                self._gate.log_context(),
                logging.ERROR,
                "paid handler would run without a verified payment: refused",
                resultType=result.type,
            )
            return gate_error(502, MSG_NOT_VERIFIED)
        self._result = result
        return None

    def _payment_error(self, response: HTTPResponseInstructions | None) -> GateResponse:
        state = self.state.snapshot() if self.state is not None else None
        if state is not None and state.prior is not None:
            # A recorded payment may have moved: the SDK's 402 (a fresh offer, or "expired")
            # would invite a second payment. The payer is told not to pay again.
            event(
                self._gate.log_context(),
                logging.WARNING,
                "recorded payment answered by the SDK as refused: answered 503, not passed through",
                canonicalHash=state.identity.canonical_hash,
            )
            return outcome_unknown(state.prior.update_id or state.identity.prepared_tx_hash)
        if state is not None and state.verify_unavailable:
            # The SDK answers 402 for a /verify outage; a 402 tells the payer the
            # payment was refused, and a client may pay again. It is a 503.
            return gate_error(503, MSG_VERIFY_UNAVAILABLE)
        if response is None:
            return gate_error(402, "Payment required")
        return convert_instructions(response)

    # -- settle -------------------------------------------------------------

    def finish(self, handler: HandlerResponse) -> GateResponse:
        """Apply the status policy to the handler's answer, settle when it is a success,
        and build the response to write. Only called after ``verify`` returned None."""
        state, result = self.state, self._result
        assert state is not None and result is not None and result.payment_payload is not None
        log = self._gate.log_context()
        status = handler.status
        prepared = state.identity.prepared_tx_hash
        refusal = (
            status < 200
            or status > MAX_HTTP_STATUS
            or status >= 400
            or (status >= 300 and not self._gate.settle_on_redirect)
        )
        if refusal and state.prior is not None:
            # The retry of a payment that may already have moved: a bare 4xx, 5xx, redirect or
            # unusable answer would hide the charge and invite a second payment. The record is
            # kept, and the payer is told not to pay again.
            event(
                log,
                logging.WARNING,
                "paid handler refused the retry of a recorded payment: answered 503, not passed through",
                handlerStatus=status,
                canonicalHash=state.identity.canonical_hash,
            )
            return outcome_unknown(state.prior.update_id or prepared)
        if status < 200 or status > MAX_HTTP_STATUS:
            event(
                log,
                logging.ERROR,
                "paid handler wrote an invalid status: not settled",
                status=status,
            )
            return gate_error(502, MSG_INVALID_STATUS)
        if status >= 400:
            # Not settled: the handler's answer passes through unchanged.
            return GateResponse(status, list(handler.headers), handler.body)
        if status >= 300 and not self._gate.settle_on_redirect:
            event(
                log, logging.WARNING, "paid route answered a redirect: not settled", status=status
            )
            return gate_error(502, MSG_REDIRECT_NOT_CHARGED)
        return self._settle(handler, result)

    def _settle(self, handler: HandlerResponse, result: HTTPProcessResult) -> GateResponse:
        state = self.state
        server = self._gate.http_server
        assert state is not None and server is not None and result.payment_payload is not None
        assert result.payment_requirements is not None
        current = CURRENT_STATE.set(state)
        try:
            settled = server.process_settlement(
                result.payment_payload,
                result.payment_requirements,
                context=self._ctx,
                declared_extensions=result.declared_extensions,
                transport_context=HTTPTransportContext(request=self._ctx),
                before_handler_settlement=result.before_handler_settlement,
            )
            if settled.success:
                return self._paid_response(handler, settled)
            return self._failure(settled)
        except Exception as error:
            self._release_claim(error)
            return self._internal_error("settle", error, charged=None)
        finally:
            CURRENT_STATE.reset(current)

    def _paid_response(
        self, handler: HandlerResponse, settled: ProcessSettleResult
    ) -> GateResponse:
        """The handler's answer plus PAYMENT-RESPONSE, ``Cache-Control: private`` merged into the handler's value."""
        status = handler.status
        headers: Headers = []
        cache_control: list[str] = []
        for name, value in handler.headers:
            lowered = name.lower()
            if lowered == HEADER_CACHE_CONTROL.lower():
                cache_control.append(value)
            elif lowered not in RESERVED_HANDLER_HEADERS and lowered not in (
                HEADER_CONTENT_LENGTH.lower(),
                HEADER_TRANSFER_ENCODING.lower(),
            ):
                headers.append((name, value))
        headers.extend(settled.headers.items())
        headers.append(
            (HEADER_CACHE_CONTROL, with_private_cache_control(", ".join(cache_control) or None))
        )
        body = handler.body
        if status in (STATUS_NO_CONTENT, STATUS_NOT_MODIFIED):
            body = b""  # no body is allowed: drop it so the write cannot fail after the payer has the answer
        else:
            headers.append((HEADER_CONTENT_LENGTH, str(len(body))))
        return GateResponse(status, headers, body, delivered_paid=True)

    def _failure(self, settled: ProcessSettleResult) -> GateResponse:
        """Decide the status and body of every settle failure. The handler's headers never reach it."""
        state = self.state
        assert state is not None
        s = state.snapshot()
        prepared = s.identity.prepared_tx_hash or s.last_tx
        if s.not_charged and s.prior is None:
            return gate_error(503, s.not_charged_message)
        if s.not_charged:
            return outcome_unknown((s.prior.update_id if s.prior else "") or prepared)
        if s.settle_unknown:
            return outcome_unknown(prepared)
        if s.withheld:
            return outcome_unknown(s.withheld_tx)
        if s.redeemed:
            return already_redeemed(s.redeemed_tx)
        if not s.refused and (s.settle_calls > 0 or s.prior is not None):
            # A /settle call was made (or the payment is recorded) and no definite refusal
            # was recorded: whatever the SDK says, the money may have moved. Never a 402.
            return outcome_unknown(prepared)
        receipt = [(k, v) for k, v in settled.headers.items() if k.lower() == "payment-response"]
        return gate_error(402, MSG_SETTLEMENT_FAILED, receipt)

    def _internal_error(
        self, phase: str, error: Exception, *, charged: bool | None
    ) -> GateResponse:
        """An unexpected error. Never a 402: if a /settle call was made the money may
        have moved, so the payer is told not to pay again."""
        state = self.state
        event(
            self._gate.log_context(),
            logging.ERROR,
            "unexpected error in the paid request",
            phase=phase,
            errorType=type(error).__name__,
            canonicalHash=state.identity.canonical_hash if state else "",
        )
        if isinstance(error, MissingStateError):
            return gate_error(502, MSG_NOT_VERIFIED)
        if state is not None and state.prior is not None and not state.snapshot().refused:
            # A recorded payment may have moved: never "nothing was charged".
            return outcome_unknown(state.prior.update_id or state.identity.prepared_tx_hash)
        if state is not None and charged is None:
            snapshot = state.snapshot()
            if snapshot.settle_calls > 0 and not snapshot.refused:
                return outcome_unknown(snapshot.identity.prepared_tx_hash)
        return gate_error(500, MSG_INTERNAL)

    # -- delivery -----------------------------------------------------------

    def release_undelivered(self, reason: str) -> None:
        """Give the claim back after the response could not be written, so the retry of
        the same payment is served once."""
        self._release_claim(None, reason)

    def _release_claim(self, error: Exception | None, reason: str = "") -> None:
        state = self.state
        if state is None:
            return
        with state.mutex:
            update_id, state.claimed_tx = state.claimed_tx, ""
        if not update_id:
            return
        event(
            self._gate.log_context(),
            logging.ERROR,
            "response not delivered after claim: claim released, a retry of the same payment is served once",
            canonicalHash=state.identity.canonical_hash,
            updateId=update_id,
            reason=reason or type(error).__name__,
        )
        self._gate.ledger.unclaim(update_id, state.token)


def convert_instructions(response: HTTPResponseInstructions) -> GateResponse:
    """The SDK's own 402 (offer, refusal) as a complete answer."""
    headers: Headers = list(response.headers.items())
    if response.is_html:
        body = (
            response.body.encode("utf-8")
            if isinstance(response.body, str)
            else bytes(response.body or b"")
        )
    else:
        body = json.dumps(response.body or {}).encode("utf-8")
    return GateResponse(response.status, headers, body)
