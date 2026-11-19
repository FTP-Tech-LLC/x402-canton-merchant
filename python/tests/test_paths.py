"""Rule 15: path spellings. Each is either charged (402 without payment) or not served with paid content."""

from __future__ import annotations

import pytest

from tests.mock_facilitator import MockFacilitator
from tests.support import Harness

SPELLINGS = [
    "GET /paid/",
    "GET //paid",
    "GET /PAID",
    "GET /%70aid",
    "GET /./paid",
    "GET /x/../paid",
    "GET /paid?x=1",
    "GET /paid;a=b",
    "GET /paid%2F",
    "GET /%2e/paid",
    "GET /paid.",
    "GET /paid%00",
    "HEAD /paid",
]


@pytest.mark.parametrize("line", SPELLINGS)
def test_unpaid_spellings_are_never_served_free(harness: Harness, line: str) -> None:
    status, headers, body = harness.raw(line)
    assert status in (402, 404, 405, 307, 308), (line, status)
    assert harness.runs == 0, f"{line} served the paid handler without payment"
    assert b'"data"' not in body


@pytest.mark.parametrize("line", SPELLINGS)
def test_a_paid_spelling_is_charged_or_not_served(
    harness: Harness, facilitator: MockFacilitator, line: str
) -> None:
    status, headers, body = harness.raw(line, f"PAYMENT-SIGNATURE: {harness.header(1)}\r\n")
    if harness.runs:
        # The handler ran: this spelling must be one the gate charges for.
        assert status == 200 and facilitator.count("/settle") == 1, (line, status)
    else:
        assert facilitator.count("/settle") == 0 and status != 200, (line, status)


def test_head_with_a_payment_is_refused_and_does_not_spend_the_payment(
    harness: Harness, facilitator: MockFacilitator
) -> None:
    """A HEAD cannot carry the paid response: it must not settle or claim."""
    status, headers, body = harness.raw("HEAD /paid", f"PAYMENT-SIGNATURE: {harness.header(1)}\r\n")
    assert status == 405 and headers["allow"] == "GET"
    assert facilitator.count("/verify") == 0 and facilitator.count("/settle") == 0
    assert harness.pay(1).status_code == 200  # the same payment still works with GET


def test_head_without_a_payment_is_gated_like_get(harness: Harness) -> None:
    status, headers, body = harness.raw("HEAD /paid")
    assert status == 402 and "payment-required" in headers
    assert harness.runs == 0


def test_the_case_variant_is_gated_by_the_sdk_itself(harness: Harness) -> None:
    status, headers, body = harness.raw("GET /PAID")
    assert status in (402, 404)
