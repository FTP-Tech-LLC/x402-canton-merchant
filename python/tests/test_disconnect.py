"""A payer who leaves, on a real server (uvicorn for ASGI, werkzeug for WSGI)."""

from __future__ import annotations

import time
from collections.abc import Callable

import pytest

from tests.mock_facilitator import PAYER as PAYER_PARTY
from tests.mock_facilitator import Answer, MockFacilitator
from tests.support import Harness, Out, Seen, tx_hash, wait_until


@pytest.mark.parametrize("http_impl", ["h11", "httptools"])
def test_payer_leaves_during_settle_then_the_retry_is_served_once(
    make: Callable[..., Harness], facilitator: MockFacilitator, http_impl: str
) -> None:
    """``http_impl`` is uvicorn's HTTP parser (ASGI only); both must report the disconnect."""
    harness = make(http=http_impl)
    facilitator.settle_queue.append(
        Answer(
            200,
            {
                "success": True,
                "transaction": "1220u" + tx_hash(1)[-60:],
                "network": "canton:mainnet",
                "payer": PAYER_PARTY,
            },
            delay=0.6,
        )
    )
    harness.send_and_leave(1)
    assert wait_until(lambda: harness.server.registry.__len__() == 0, 10)
    assert harness.runs == 1  # the handler ran, the body went nowhere
    settles = facilitator.count("/settle")
    verifies = facilitator.count("/verify")

    retry = harness.pay(1)
    assert retry.status_code == 200, retry.text
    assert harness.runs == 2  # served once on the retry
    assert (
        facilitator.count("/settle") == settles
    )  # the recorded updateId is delivered, no second /settle
    assert facilitator.count("/verify") == verifies  # and no second /verify (it would say expired)
    assert harness.seen[-1].payer == PAYER_PARTY
    assert harness.seen[-1].retry is True
    assert harness.receipt(retry) is not None

    replay = harness.pay(1)
    assert replay.status_code == 402
    assert replay.json() == {"error": "payment_already_redeemed"}
    assert harness.runs == 2


def test_payer_gone_before_settle_is_not_charged_and_the_retry_works(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make()

    def slow_handler(seen: Seen) -> Out:
        time.sleep(0.6)
        return Out()

    harness.handler = slow_handler
    import socket

    request = (
        f"GET /paid HTTP/1.1\r\nHost: x\r\nPAYMENT-SIGNATURE: {harness.header(1)}\r\nConnection: close\r\n\r\n"
    ).encode()
    sock = socket.create_connection(("127.0.0.1", harness.port), timeout=10)
    sock.sendall(request)
    assert wait_until(lambda: harness.runs == 1, 10)
    sock.shutdown(socket.SHUT_RDWR)
    sock.close()
    assert wait_until(lambda: len(harness.server.registry) == 0, 10)
    assert facilitator.count("/settle") == 0  # nothing was submitted: the payment expires unused

    harness.handler = lambda seen: Out()
    retry = harness.pay(1)
    assert retry.status_code == 200
    assert facilitator.count("/settle") == 1
