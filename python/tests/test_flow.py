"""Rules 4, 5, 11, 13, 15: the flow around the paid handler and its status."""

from __future__ import annotations

import base64
import json
from collections.abc import Callable

import pytest

from ftptech_x402_canton import payer_from
from tests import inproc
from tests.mock_facilitator import FEE_PAYER, MockFacilitator
from tests.support import Harness, Out, Seen


def test_settle_runs_after_the_handler_never_before(
    harness: Harness, facilitator: MockFacilitator
) -> None:
    seen_settles: list[int] = []

    def handler(seen: Seen) -> Out:
        seen_settles.append(facilitator.count("/settle"))
        return Out()

    harness.handler = handler
    assert harness.pay().status_code == 200
    assert seen_settles == [0]  # /verify ran, /settle had not
    assert facilitator.paths()[-2:] == ["/verify", "/settle"]
    assert facilitator.count("/settle") == 1


@pytest.mark.parametrize("status", [200, 201, 202, 204])
def test_every_handler_status_below_300_is_settled(
    harness: Harness, facilitator: MockFacilitator, status: int
) -> None:
    harness.handler = lambda seen: Out(status, b"" if status == 204 else b"body")
    response = harness.pay()
    assert response.status_code == status
    assert facilitator.count("/settle") == 1
    assert harness.receipt(response)["success"] is True  # type: ignore[index]


def test_204_never_carries_a_body_and_the_claim_stays(
    harness: Harness, facilitator: MockFacilitator
) -> None:
    """A handler body on a 204 must not make the write fail after the payer has the receipt."""
    harness.handler = lambda seen: Out(204, b"unexpected body")
    response = harness.pay()
    assert response.status_code == 204
    assert response.content == b""
    replay = harness.pay()
    assert replay.status_code == 402  # delivered: the claim was kept
    assert harness.runs == 1


@pytest.mark.parametrize("status", [400, 401, 404, 429, 500, 503])
def test_handler_errors_are_not_settled_and_pass_through_unchanged(
    harness: Harness, facilitator: MockFacilitator, status: int
) -> None:
    harness.handler = lambda seen: Out(
        status,
        b'{"why":"handler"}',
        {"Content-Type": "application/json", "X-Mine": "1"},
        cookies=True,
    )
    response = harness.pay()
    assert response.status_code == status
    assert response.json() == {"why": "handler"}
    assert response.headers["x-mine"] == "1"
    assert "session=secret" in response.headers.get("set-cookie", "")
    assert facilitator.count("/settle") == 0
    assert "payment-response" not in response.headers


def test_handler_that_raises_is_not_settled_and_releases_the_lock(
    harness: Harness, facilitator: MockFacilitator
) -> None:
    def boom(seen: Seen) -> Out:
        raise RuntimeError("boom")

    harness.handler = boom
    assert harness.pay().status_code == 500
    assert facilitator.count("/settle") == 0
    harness.handler = lambda seen: Out()
    assert harness.pay().status_code == 200  # the in-flight lock was released
    assert facilitator.count("/settle") == 1


def test_redirect_from_a_paid_handler_is_not_charged_and_becomes_502(
    harness: Harness, facilitator: MockFacilitator
) -> None:
    harness.handler = lambda seen: Out(
        302, b"", {"Location": "https://elsewhere.example/", "Set-Cookie": "a=b"}
    )
    response = harness.pay()
    assert response.status_code == 502
    assert response.json() == {"error": "paid route answered a redirect; nothing was charged"}
    assert "location" not in response.headers and "set-cookie" not in response.headers
    assert facilitator.count("/settle") == 0


def test_redirect_is_settled_when_settle_on_redirect_is_set(
    make: Callable[..., Harness], facilitator: MockFacilitator
) -> None:
    harness = make(settle_on_redirect=True)
    harness.handler = lambda seen: Out(302, b"", {"Location": "https://elsewhere.example/"})
    response = harness.client.get(
        "/paid", headers={"PAYMENT-SIGNATURE": harness.header()}, follow_redirects=False
    )
    assert response.status_code == 302
    assert response.headers["location"] == "https://elsewhere.example/"
    assert facilitator.count("/settle") == 1


def test_handler_sees_the_proven_payer_and_can_refuse_without_charging(
    harness: Harness, facilitator: MockFacilitator
) -> None:
    def limiter(seen: Seen) -> Out:
        if seen.payer == facilitator.payer:
            return Out(429, b'{"error":"limit"}')
        return Out()

    harness.handler = limiter
    response = harness.pay()
    assert harness.seen[0].payer == facilitator.payer
    assert response.status_code == 429
    assert facilitator.count("/settle") == 0  # nobody was charged


def test_payer_from_outside_a_paid_request_is_none() -> None:
    assert payer_from({}) is None
    assert payer_from(None) is None
    assert payer_from(object()) is None


def test_payer_of_an_invalid_verify_is_never_used(
    harness: Harness, facilitator: MockFacilitator
) -> None:
    from tests.mock_facilitator import Answer

    facilitator.verify_queue.append(
        Answer(
            200,
            {
                "isValid": False,
                "invalidReason": "invalid_exact_canton_expired",
                "payer": "victim::1220" + "ee" * 32,
            },
        )
    )
    response = harness.pay()
    assert response.status_code == 402
    assert harness.runs == 0  # the handler never ran, so it never saw a forged payer
    assert "victim" not in response.text


def test_paid_responses_are_cache_private_merged_with_the_handlers_value(harness: Harness) -> None:
    harness.handler = lambda seen: Out(200, b"x", {"Cache-Control": "max-age=60"})
    assert harness.pay().headers["cache-control"] == "max-age=60, private"
    harness.handler = lambda seen: Out(200, b"x", {})
    assert harness.pay(2).headers["cache-control"] == "private"


def test_paid_response_keeps_the_handlers_headers_and_adds_the_receipt(harness: Harness) -> None:
    harness.handler = lambda seen: Out(
        200, b'{"a":1}', {"Content-Type": "application/json", "X-Mine": "yes"}, cookies=True
    )
    response = harness.pay()
    assert response.headers["x-mine"] == "yes"
    assert "session=secret" in response.headers["set-cookie"]
    assert response.headers["content-length"] == "7"
    receipt = harness.receipt(response)
    assert (
        receipt is not None
        and receipt["success"] is True
        and receipt["transaction"].startswith("1220u")
    )


def test_the_handler_cannot_forge_the_payment_response_header(harness: Harness) -> None:
    forged = base64.b64encode(
        json.dumps({"success": True, "transaction": "forged"}).encode()
    ).decode()
    harness.handler = lambda seen: Out(200, b"x", {"PAYMENT-RESPONSE": forged})
    receipt = harness.receipt(harness.pay())
    assert receipt is not None and receipt["transaction"] != "forged"


def test_unpaid_routes_pass_through_untouched(
    harness: Harness, facilitator: MockFacilitator
) -> None:
    response = harness.client.get("/free")
    assert response.status_code == 200 and response.json() == {"free": "yes"}
    assert facilitator.calls_to("/verify") == [] and facilitator.calls_to("/settle") == []


def test_only_the_configured_method_is_charged(
    harness: Harness, facilitator: MockFacilitator
) -> None:
    """The gate charges ``GET /paid`` only. A handler registered for other methods of
    the same path serves them free: register the paid handler with the route's method."""
    assert harness.client.get("/paid").status_code == 402
    assert harness.client.post("/paid").status_code == 200
    assert harness.runs == 1


def test_the_402_offer_is_the_merchants_own_terms(harness: Harness) -> None:
    offer = harness.offer()
    assert offer["scheme"] == "exact" and offer["network"] == "canton:mainnet"
    assert offer["asset"] == "CC" and offer["amount"] == "100000000"
    assert offer["extra"]["assetTransferMethod"] == "transfer-factory"
    assert offer["extra"]["feePayer"] == FEE_PAYER
    assert "paymentFlow" not in offer["extra"]  # authorization flow only: upfront is never offered
    assert offer["maxTimeoutSeconds"] == 120 and offer["extra"]["executeBeforeSeconds"] == 120


@pytest.mark.parametrize("status", [1000, 99, 0])
def test_invalid_handler_status_is_not_settled_asgi(
    facilitator: MockFacilitator, status: int
) -> None:
    import asyncio

    async def run() -> int:
        server = inproc.build_server(facilitator)
        client = await inproc.asgi_client(server, inproc.asgi_app(status))
        async with client:
            first = await client.get("/paid")
            accepted = json.loads(base64.b64decode(first.headers["payment-required"]))["accepts"][0]
            response = await client.get("/paid", headers=inproc.header_for(accepted))
        server.close()
        return response.status_code

    assert asyncio.run(run()) == 502
    assert facilitator.count("/settle") == 0


def test_invalid_handler_status_is_not_settled_wsgi(facilitator: MockFacilitator) -> None:
    server = inproc.build_server(facilitator)
    with inproc.wsgi_client(server, inproc.wsgi_app("1000 Weird")) as client:
        accepted = inproc.offer_via(client)
        response = client.get("/paid", headers=inproc.header_for(accepted))
    server.close()
    assert response.status_code == 502
    assert facilitator.count("/settle") == 0


def test_settlement_overrides_are_never_honoured(
    harness: Harness, facilitator: MockFacilitator
) -> None:
    """The signed amount is fixed: a handler cannot ask the SDK to settle a different one."""
    harness.handler = lambda seen: Out(200, b"x", {"SETTLEMENT-OVERRIDES": '{"amount":"1"}'})
    response = harness.pay(1)
    assert response.status_code == 200
    assert "settlement-overrides" not in response.headers
    sent = facilitator.calls_to("/settle")[0].json()["paymentRequirements"]
    assert sent["amount"] == "100000000"
