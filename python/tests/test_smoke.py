from __future__ import annotations

from tests.support import Harness


def test_unpaid_gets_the_402_offer(harness: Harness) -> None:
    offer = harness.offer()
    assert offer["network"] == "canton:mainnet"
    assert offer["amount"] == "100000000"


def test_paid_request_is_delivered_once(harness: Harness) -> None:
    response = harness.pay()
    assert response.status_code == 200, response.text
    assert response.json() == {"data": 42}
    assert harness.runs == 1
