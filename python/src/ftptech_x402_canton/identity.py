"""Payment identity: which payment a PAYMENT-SIGNATURE header is, and what bytes it carries."""

from __future__ import annotations

import hashlib
import json
import re
import secrets
from dataclasses import dataclass
from enum import Enum

from pydantic import BaseModel
from x402.http.utils import decode_payment_signature_header
from x402.schemas import PaymentPayload, PaymentRequirements

# The Canton hash prefix (sha-256, 32 bytes) that some clients keep on the
# prepared transaction hash and others strip.
MULTIHASH_PREFIX = "1220"
PREFIXED_HASH_LENGTH = 68  # 4 + 64 hex characters
TOKEN_BYTES = 16

# The only shape a canonical hash may have. It also bounds the length of the key
# the client's value becomes in a store.
_CANONICAL_HASH_RE = re.compile(r"[0-9a-f]{64}")


def canonical_hash(prepared_tx_hash: str) -> str:
    """Normalize a preparedTxHash so every spelling of one payment maps to one key.

    Trimmed, lower-case, and a leading ``1220`` removed only when the value is
    68 characters long.
    """
    value = prepared_tx_hash.strip().lower()
    if len(value) == PREFIXED_HASH_LENGTH and value.startswith(MULTIHASH_PREFIX):
        return value[len(MULTIHASH_PREFIX) :]
    return value


@dataclass(frozen=True)
class PaymentIdentity:
    prepared_tx_hash: str  # trimmed, lower-case, as the facilitator reports it
    canonical_hash: str


class HeaderOutcome(Enum):
    #: The SDK cannot read it either, so the SDK answers (an unpaid 402).
    UNREADABLE = "unreadable"
    #: The SDK reads it and it carries a usable hash.
    READABLE = "readable"
    #: The SDK reads it but ``payload.preparedTxHash`` is missing, not a string, or
    #: not a 64-character hash. The facilitator would refuse it too.
    NO_HASH = "no_hash"


def identity_from_payload(payload: PaymentPayload) -> PaymentIdentity | None:
    """The identity of a decoded payload, by the exact key ``preparedTxHash``.

    The key is read exactly as the SDK forwards the payload: a parser that matched
    keys loosely would give the gate a different key than the facilitator uses,
    and the lock and the delivered check would be bypassed.
    """
    prepared = payload.payload.get("preparedTxHash")
    if not isinstance(prepared, str):
        return None
    canonical = canonical_hash(prepared)
    if _CANONICAL_HASH_RE.fullmatch(canonical) is None:
        return None
    return PaymentIdentity(prepared.strip().lower(), canonical)


def parse_payment_header(header: str) -> tuple[HeaderOutcome, PaymentIdentity | None]:
    """Derive the payment identity the way the SDK reads the header (same decoder)."""
    try:
        decoded = decode_payment_signature_header(header)
    except Exception:  # the SDK treats any undecodable header as "no payment"
        return HeaderOutcome.UNREADABLE, None
    if not isinstance(decoded, PaymentPayload):
        return HeaderOutcome.UNREADABLE, None  # a V1 payload: the V2 server treats it as no payment
    identity = identity_from_payload(decoded)
    if identity is None:
        return HeaderOutcome.NO_HASH, None
    return HeaderOutcome.READABLE, identity


def _canonical_json(model: BaseModel) -> bytes:
    data = model.model_dump(mode="json", by_alias=True, exclude_none=True)
    return json.dumps(data, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()


def request_body(payload: PaymentPayload, requirements: PaymentRequirements) -> bytes:
    """The exact bytes sent to /verify and /settle. Built once so a retry sends the identical bytes."""
    body = {
        "x402Version": payload.x402_version,
        "paymentPayload": json.loads(_canonical_json(payload)),
        "paymentRequirements": json.loads(_canonical_json(requirements)),
    }
    return json.dumps(body, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()


#: The ``extra`` fields the facilitator injects into the 402 and the SDK's own match leaves out.
DYNAMIC_EXTRA_FIELDS = ("feePayer", "synchronizerId")


def _requirements_json(requirements: PaymentRequirements, *, with_dynamic: bool) -> bytes:
    data = requirements.model_dump(mode="json", by_alias=True, exclude_none=True)
    if not with_dynamic and isinstance(data.get("extra"), dict):
        data["extra"] = {k: v for k, v in data["extra"].items() if k not in DYNAMIC_EXTRA_FIELDS}
    return json.dumps(data, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()


def _digest(payload: PaymentPayload, requirements_json: bytes) -> str:
    sha = hashlib.sha256()
    sha.update(_canonical_json(payload))
    sha.update(b"\x00")
    sha.update(requirements_json)
    return sha.hexdigest()


def payment_digest(payload: PaymentPayload, requirements: PaymentRequirements) -> str:
    """Binds a record to the exact payload and requirements the SDK forwards to the facilitator.

    The requirements are taken without ``extra.feePayer`` and ``extra.synchronizerId``,
    the fields the facilitator injects: a /supported read after a fee-payer rotation (on
    another instance, or after a restart) must not make a withheld payment look like
    a different one.
    """
    return _digest(payload, _requirements_json(requirements, with_dynamic=False))


def legacy_payment_digest(payload: PaymentPayload, requirements: PaymentRequirements) -> str:
    """The digest over the requirements as sent, accepted for records written before the dynamic fields were left out."""
    return _digest(payload, _requirements_json(requirements, with_dynamic=True))


def digest_matches(
    recorded: str, payload: PaymentPayload, requirements: PaymentRequirements
) -> bool:
    """Whether a record was written for these bytes."""
    return bool(recorded) and recorded in (
        payment_digest(payload, requirements),
        legacy_payment_digest(payload, requirements),
    )


def new_token() -> str:
    """A per-request random value: the owner of a lock or a claim."""
    return secrets.token_hex(TOKEN_BYTES)
