"""Everything the gate itself answers: wording, headers and the shape of each answer."""

from __future__ import annotations

import json
from dataclasses import dataclass, field

from x402.http.constants import PAYMENT_REQUIRED_HEADER, PAYMENT_RESPONSE_HEADER
from x402.http.utils import encode_payment_response_header
from x402.schemas import SettleResponse

from .mechanisms.canton import CANTON_MAINNET_CAIP2, ERR_SETTLEMENT_PENDING


def with_private_cache_control(value: str | None) -> str:
    """Append the ``private`` directive to a Cache-Control value (a copy of the SDK's helper: it is not exported)."""
    if not value:
        return "private"
    if "private" in [directive.strip().lower() for directive in value.split(",")]:
        return value
    return f"{value}, private"


#: The Retry-After the gate sends with its 503 answers.
RETRY_AFTER_SECONDS = "3"

HEADER_RETRY_AFTER = "Retry-After"
HEADER_CACHE_CONTROL = "Cache-Control"
HEADER_CONTENT_TYPE = "Content-Type"
HEADER_CONTENT_LENGTH = "Content-Length"
HEADER_TRANSFER_ENCODING = "Transfer-Encoding"
CACHE_CONTROL_NO_STORE = "no-store"
CONTENT_TYPE_JSON = "application/json"

#: What the facilitator answers for a repeat of a settled payment. It is not in
#: the upstream spec table, so it lives in this layer.
ERR_REASON_ALREADY_SETTLED = "invalid_exact_canton_payment_already_settled"
#: What the gate answers for a delivered payment.
ERR_REASON_ALREADY_REDEEMED = "payment_already_redeemed"

# Gate answers. The wording of the unknown-outcome and verify answers is the
# contract with clients: "do NOT pay again" tells a client not to start a new
# payment, and "retry the same payment" tells it to resend the same signature.
MSG_OUTCOME_UNKNOWN = "payment outcome unknown: do NOT pay again; retry the same payment"
MSG_VERIFY_UNAVAILABLE = "verification unavailable: nothing was settled; retry the same payment"
MSG_PAYMENT_IN_PROGRESS = "payment in progress: retry the same payment"
MSG_STORE_UNAVAILABLE = "payment state unavailable: do NOT pay again; retry the same payment"
MSG_STORE_FULL = "payment state unavailable: nothing was charged; retry the same payment"
MSG_NOT_VERIFIED = "payment not verified; nothing was charged"
MSG_HEAD_WITH_PAYMENT = "send the payment with GET: a HEAD cannot receive the paid response"
MSG_REDIRECT_NOT_CHARGED = "paid route answered a redirect; nothing was charged"
MSG_NOT_FOUND = "not found"
MSG_NO_HASH = "payment header carries no valid preparedTxHash"
MSG_PAYER_LEFT = "payer left before settle: nothing was charged; retry the same payment"
MSG_INVALID_STATUS = "paid handler wrote an invalid status; nothing was charged"
# Said before the payment record is read, so it never claims "nothing was charged": the retry of a
# recorded payment gets the same words.
MSG_BUSY = "payment service is busy: if you already paid, do NOT pay again; retry the same payment"
MSG_NOT_READY = (
    "payment service is starting: if you already paid, do NOT pay again; retry the same payment"
)
MSG_INTERNAL = "internal error: nothing was charged; retry the same payment"
MSG_SETTLEMENT_FAILED = "settlement_failed"

#: The only headers a gate-made error answer keeps.
GATE_ALLOWED_HEADERS = frozenset(
    name.lower()
    for name in (
        PAYMENT_RESPONSE_HEADER,
        PAYMENT_REQUIRED_HEADER,
        HEADER_RETRY_AFTER,
        HEADER_CACHE_CONTROL,
        HEADER_CONTENT_TYPE,
    )
)

#: Headers a paid handler may not set on its own: the gate owns them.
RESERVED_HANDLER_HEADERS = frozenset(
    (
        PAYMENT_RESPONSE_HEADER.lower(),
        PAYMENT_REQUIRED_HEADER.lower(),
        "x-payment-response",
        "settlement-overrides",
        "x-settlement-overrides",
    )
)

Headers = list[tuple[str, str]]


@dataclass
class GateResponse:
    """A complete HTTP answer, held in memory until the driver writes it."""

    status: int
    headers: Headers = field(default_factory=list)
    body: bytes = b""
    #: True for a paid response whose updateId this request claimed: if the write
    #: fails the claim is released so the retry is served once.
    delivered_paid: bool = False


def gate_error(status: int, message: str, extra_headers: Headers | None = None) -> GateResponse:
    """A JSON gate answer: no-store, and Retry-After for 503. Never carries handler headers."""
    headers: Headers = [
        (HEADER_CONTENT_TYPE, CONTENT_TYPE_JSON),
        (HEADER_CACHE_CONTROL, CACHE_CONTROL_NO_STORE),
    ]
    if status == 503:
        headers.append((HEADER_RETRY_AFTER, RETRY_AFTER_SECONDS))
    headers.extend(extra_headers or [])
    return GateResponse(status, headers, json.dumps({"error": message}).encode())


def payment_response_header(response: SettleResponse) -> tuple[str, str]:
    return PAYMENT_RESPONSE_HEADER, encode_payment_response_header(response)


def outcome_unknown(transaction: str) -> GateResponse:
    """503: money may have moved, so the payer is told not to pay again and to resend the same payment."""
    receipt = SettleResponse(
        success=False,
        error_reason=ERR_SETTLEMENT_PENDING,
        transaction=transaction,
        network=CANTON_MAINNET_CAIP2,
    )
    return gate_error(503, MSG_OUTCOME_UNKNOWN, [payment_response_header(receipt)])


def already_redeemed(update_id: str) -> GateResponse:
    receipt = SettleResponse(
        success=False,
        error_reason=ERR_REASON_ALREADY_REDEEMED,
        transaction=update_id,
        network=CANTON_MAINNET_CAIP2,
    )
    return gate_error(402, ERR_REASON_ALREADY_REDEEMED, [payment_response_header(receipt)])


def scrub_to_gate_headers(headers: Headers) -> Headers:
    """Keep only the gate's own headers."""
    return [(k, v) for k, v in headers if k.lower() in GATE_ALLOWED_HEADERS]
