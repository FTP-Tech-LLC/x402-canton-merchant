from __future__ import annotations

import base64
import json
from typing import Any

import pytest
from x402.schemas import PaymentPayload, PaymentRequirements

from ftptech_x402_canton.identity import (
    HeaderOutcome,
    canonical_hash,
    identity_from_payload,
    new_token,
    parse_payment_header,
    payment_digest,
    request_body,
)
from tests.mock_facilitator import DSO
from tests.support import payment_header

H = "ab" * 32
ACCEPTED: dict[str, Any] = {
    "scheme": "exact", "network": "canton:mainnet", "asset": "CC", "amount": "100000000",
    "payTo": "merchant::1220" + "bb" * 32, "maxTimeoutSeconds": 120,
    "extra": {"instrumentId": {"admin": DSO, "id": "Amulet"}},
}  # fmt: skip


@pytest.mark.parametrize(
    ("raw", "expected"),
    [
        (H, H),
        (H.upper(), H),
        ("  " + H + "\t", H),
        ("1220" + H, H),  # 68 characters: the multihash prefix goes
        ("1220" + H.upper(), H),
        (" 1220" + H + " ", H),
        ("1220" + H[:-2], "1220" + H[:-2]),  # 66 characters: not a prefixed hash
        ("12" + H, "12" + H),  # 66 characters
        ("1220" + "1220" + H, "1220" + "1220" + H),  # 72 characters: nothing is stripped
        ("", ""),
    ],
)
def test_canonical_hash_spellings(raw: str, expected: str) -> None:
    assert canonical_hash(raw) == expected


def test_a_hash_that_starts_with_1220_and_is_64_long_is_kept_whole() -> None:
    value = "1220" + "cd" * 30
    assert len(value) == 64 and canonical_hash(value) == value


def test_parse_payment_header_outcomes() -> None:
    outcome, identity = parse_payment_header(payment_header(ACCEPTED, H))
    assert outcome is HeaderOutcome.READABLE and identity is not None
    assert identity.canonical_hash == H and identity.prepared_tx_hash == H
    spelled = parse_payment_header(payment_header(ACCEPTED, ("1220" + H).upper()))[1]
    assert (
        spelled is not None
        and spelled.canonical_hash == H
        and spelled.prepared_tx_hash == "1220" + H
    )


@pytest.mark.parametrize(
    "header",
    [
        "",
        "!!!",
        base64.b64encode(b"x").decode(),
        base64.b64encode(b"[]").decode(),
        base64.b64encode(b'"s"').decode(),
    ],
)
def test_unreadable_headers_go_to_the_sdk(header: str) -> None:
    assert parse_payment_header(header) == (HeaderOutcome.UNREADABLE, None)


def test_a_v1_payload_is_unreadable_for_the_v2_server() -> None:
    v1 = base64.b64encode(
        json.dumps({"x402Version": 1, "scheme": "exact", "network": "base", "payload": {}}).encode()
    ).decode()
    assert parse_payment_header(v1)[0] is HeaderOutcome.UNREADABLE


@pytest.mark.parametrize("payload", [{}, {"preparedTxHash": None}, {"preparedTxHash": 5}, {"preparedTxHash": ["a"]},
                                      {"preparedTxHash": "short"}, {"preparedTxHash": "g" * 64}, {"PREPAREDTXHASH": H},
                                      {"prepared_tx_hash": H}, {"preparedTxHash": H + "00"}])  # fmt: skip
def test_no_usable_hash(payload: dict[str, Any]) -> None:
    body = {"x402Version": 2, "accepted": ACCEPTED, "payload": payload}
    header = base64.b64encode(json.dumps(body).encode()).decode()
    assert parse_payment_header(header)[0] is HeaderOutcome.NO_HASH


def make(
    signature: str = "s", amount: str = "100000000"
) -> tuple[PaymentPayload, PaymentRequirements]:
    payload = PaymentPayload.model_validate(
        {
            "x402Version": 2,
            "accepted": ACCEPTED,
            "payload": {"preparedTxHash": H, "signature": signature},
        }
    )
    requirements = PaymentRequirements.model_validate({**ACCEPTED, "amount": amount})
    return payload, requirements


def test_the_digest_binds_the_payload_and_the_requirements() -> None:
    base = payment_digest(*make())
    assert base == payment_digest(*make())  # stable
    assert base != payment_digest(*make(signature="other"))
    assert base != payment_digest(*make(amount="200000000"))
    assert len(base) == 64


def test_verify_and_settle_see_the_same_bytes() -> None:
    payload, requirements = make()
    assert request_body(payload, requirements) == request_body(payload, requirements)
    sent = json.loads(request_body(payload, requirements))
    assert sent["x402Version"] == 2 and sent["paymentPayload"]["payload"]["preparedTxHash"] == H
    assert sent["paymentRequirements"]["payTo"] == ACCEPTED["payTo"]


def test_identity_from_payload_uses_the_exact_key() -> None:
    payload, _ = make()
    identity = identity_from_payload(payload)
    assert identity is not None and identity.canonical_hash == H
    shadow = PaymentPayload.model_validate(
        {"x402Version": 2, "accepted": ACCEPTED, "payload": {"PreparedTxHash": H}}
    )
    assert identity_from_payload(shadow) is None


def test_tokens_are_random_and_long() -> None:
    tokens = {new_token() for _ in range(100)}
    assert len(tokens) == 100 and all(len(t) == 32 for t in tokens)
