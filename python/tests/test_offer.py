"""Rule 3: the 402 offer the SDK builds through the mirror scheme."""

from __future__ import annotations

import base64
import json
import os
from typing import Any

import httpx
import pytest

from ftptech_x402_canton import Route, atomic, cc, token
from ftptech_x402_canton.mechanisms.canton import InstrumentId
from ftptech_x402_canton.price import DSO_MAINNET_PARTY
from tests import inproc
from tests.mock_facilitator import FEE_PAYER, SYNCHRONIZER, MockFacilitator, default_supported
from tests.support import PAY_TO

ADMIN = "usdcx-admin::1220" + "dd" * 32


def offers(facilitator: MockFacilitator, routes: dict[str, Route]) -> dict[str, dict[str, Any]]:
    server = inproc.build_server(facilitator, routes=routes)
    result = {}
    with inproc.wsgi_client(server, inproc.wsgi_app("200 OK")) as client:
        for key in routes:
            response = client.request(key.split()[0], key.split()[1])
            assert response.status_code == 402, (key, response.text)
            result[key] = json.loads(base64.b64decode(response.headers["payment-required"]))
    server.close()
    return result


def test_offer_carries_the_canton_terms_from_supported(facilitator: MockFacilitator) -> None:
    routes = {
        "GET /paid": Route(
            pay_to=PAY_TO,
            price=cc("0.01"),
            execute_before_seconds=90,
            memo="order-42",
            description="Data",
            mime_type="application/json",
        )
    }
    required = offers(facilitator, routes)["GET /paid"]
    accepted = required["accepts"][0]
    assert required["x402Version"] == 2 and len(required["accepts"]) == 1
    assert accepted["scheme"] == "exact" and accepted["network"] == "canton:mainnet"
    assert (
        accepted["amount"] == "100000000"
        and accepted["asset"] == "CC"
        and accepted["payTo"] == PAY_TO
    )
    assert accepted["maxTimeoutSeconds"] == 90
    assert accepted["extra"] == {
        "assetTransferMethod": "transfer-factory",
        "instrumentId": {"admin": DSO_MAINNET_PARTY, "id": "Amulet"},
        "executeBeforeSeconds": 90,
        "memo": "order-42",
        "feePayer": FEE_PAYER,  # from /supported, never from merchant config
        "synchronizerId": SYNCHRONIZER,
    }
    assert (
        required["resource"]["description"] == "Data"
        and required["resource"]["mimeType"] == "application/json"
    )


def test_offer_defaults_execute_before_and_omits_the_memo(facilitator: MockFacilitator) -> None:
    accepted = offers(facilitator, {"GET /paid": Route(pay_to=PAY_TO, price=cc("1"))})["GET /paid"][
        "accepts"
    ][0]
    assert accepted["maxTimeoutSeconds"] == 120 and accepted["extra"]["executeBeforeSeconds"] == 120
    assert "memo" not in accepted["extra"]


def test_offer_for_a_token_uses_the_structured_asset(facilitator: MockFacilitator) -> None:
    supported = default_supported()
    supported["kinds"][0]["extra"]["instruments"].append(
        {"instrumentId": {"admin": ADMIN, "id": "USDCx"}}
    )
    facilitator.supported = supported
    accepted = offers(
        facilitator, {"GET /paid": Route(pay_to=PAY_TO, price=token(ADMIN, "USDCx", "1.5"))}
    )["GET /paid"]["accepts"][0]
    assert accepted["asset"] == f"{ADMIN}::USDCx" and accepted["amount"] == "15000000000"
    assert accepted["extra"]["instrumentId"] == {"admin": ADMIN, "id": "USDCx"}


def test_offer_prices_each_route_separately(facilitator: MockFacilitator) -> None:
    routes = {
        "GET /paid": Route(pay_to=PAY_TO, price=cc("0.01")),
        "POST /paid2": Route(
            pay_to=PAY_TO,
            price=atomic("12345", InstrumentId(DSO_MAINNET_PARTY, "Amulet")),
            memo="m",
        ),
    }
    result = offers(facilitator, routes)
    assert result["GET /paid"]["accepts"][0]["amount"] == "100000000"
    second = result["POST /paid2"]["accepts"][0]
    assert second["amount"] == "12345" and second["extra"]["memo"] == "m"
    assert "memo" not in result["GET /paid"]["accepts"][0]["extra"]


def test_a_merchant_cannot_choose_the_fee_payer(facilitator: MockFacilitator) -> None:
    """feePayer is always the facilitator's: the 402 never carries a merchant-chosen one."""
    accepted = offers(facilitator, {"GET /paid": Route(pay_to=PAY_TO, price=cc("0.01"))})[
        "GET /paid"
    ]["accepts"][0]
    assert accepted["extra"]["feePayer"] == FEE_PAYER


@pytest.mark.skipif(
    os.environ.get("CANTON_MERCHANT_OFFLINE") == "1", reason="CANTON_MERCHANT_OFFLINE=1"
)
def test_offer_live() -> None:
    """Against the real GET https://facilitator.ftptech.xyz/supported (a public GET; no payment is posted)."""
    try:
        answer = httpx.get("https://facilitator.ftptech.xyz/supported", timeout=15)
        answer.raise_for_status()
    except httpx.HTTPError as error:
        pytest.skip(f"offline: {type(error).__name__}")
    from ftptech_x402_canton import CantonServer, Config

    server = CantonServer(
        Config(
            routes={"GET /paid": Route(pay_to=PAY_TO, price=cc("0.01"))},
            settle_timeout=5,
            verify_timeout=5,
        )
    )
    server.start()
    with inproc.wsgi_client(server, inproc.wsgi_app("200 OK")) as client:
        response = client.get("/paid")
    server.close()
    assert response.status_code == 402
    accepted = json.loads(base64.b64decode(response.headers["payment-required"]))["accepts"][0]
    live = next(
        k
        for k in answer.json()["kinds"]
        if k["network"] == "canton:mainnet" and k["x402Version"] == 2
    )
    assert (
        accepted["network"] == "canton:mainnet"
        and accepted["scheme"] == "exact"
        and accepted["amount"] == "100000000"
    )
    assert accepted["extra"]["assetTransferMethod"] == "transfer-factory"
    assert accepted["extra"]["feePayer"] == live["extra"]["feePayer"]
    assert accepted["extra"]["synchronizerId"] == live["extra"]["synchronizerId"]
    assert accepted["extra"]["instrumentId"] == {"admin": DSO_MAINNET_PARTY, "id": "Amulet"}
