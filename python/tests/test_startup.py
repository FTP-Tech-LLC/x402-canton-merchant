"""Rule 1: startup never exits the process; /supported is retried; the kind is checked."""

from __future__ import annotations

import asyncio
import copy
import logging
import os
import time
from typing import Any

import httpx
import pytest
from x402.http import background_init
from x402.http.types import RouteConfigurationError
from x402.http.x402_http_server import x402HTTPResourceServerSync

from ftptech_x402_canton import (
    CantonConfigError,
    CantonServer,
    CantonStartupError,
    Config,
    RedisStore,
    Route,
    canton_server,
    canton_server_sync,
    cc,
    token,
)
from tests import inproc
from tests.mock_facilitator import Answer, MockFacilitator, bare, default_supported
from tests.stores import FakeRedis
from tests.support import FAST, PAID_ROUTE, PAY_TO

ADMIN = "usdcx-admin::1220" + "dd" * 32


def config(facilitator: MockFacilitator, **kwargs: Any) -> Config:
    return Config(
        routes=kwargs.pop("routes", {PAID_ROUTE: Route(pay_to=PAY_TO, price=cc("0.01"))}),
        **{**FAST, "facilitator_url": facilitator.url, "supported_wait": 0.4, **kwargs},
    )


def kind_with(**changes: Any) -> dict[str, Any]:
    supported = default_supported()
    supported["kinds"][0].update(changes)
    return supported


def test_startup_retries_supported_until_it_answers_and_reads_it_once(
    facilitator: MockFacilitator,
) -> None:
    facilitator.supported_queue.extend([bare(503), bare(502), Answer(200, raw=b"not json")])
    server = canton_server_sync(config(facilitator, supported_wait=5.0))
    assert server.ready
    assert (
        facilitator.count("/supported") == 4
    )  # one GET per attempt: the SDK's init does not read it again
    server.close()


def test_a_glitch_on_the_sdks_second_supported_read_cannot_happen(
    facilitator: MockFacilitator,
) -> None:
    server = canton_server_sync(config(facilitator))
    assert facilitator.count("/supported") == 1
    facilitator.supported_queue.append(
        bare(503)
    )  # a later glitch changes nothing: nothing reads /supported
    assert server.ready
    server.close()


def test_startup_returns_an_error_when_the_facilitator_stays_down(
    facilitator: MockFacilitator,
) -> None:
    facilitator.supported_queue.extend([bare(503)] * 100)
    started = time.monotonic()
    with pytest.raises(CantonStartupError, match="unusable after"):
        canton_server_sync(config(facilitator))
    assert time.monotonic() - started < 3


def test_startup_returns_an_error_when_the_facilitator_is_unreachable() -> None:
    cfg = Config(
        routes={PAID_ROUTE: Route(pay_to=PAY_TO, price=cc("0.01"))},
        facilitator_url="http://127.0.0.1:1",
        **{**FAST, "supported_wait": 0.3},
    )
    with pytest.raises(CantonStartupError):
        canton_server_sync(cfg)


def test_startup_does_not_hang_on_a_facilitator_that_never_answers(
    facilitator: MockFacilitator,
) -> None:
    facilitator.supported_queue.extend([Answer(200, default_supported(), delay=6.0)] * 3)
    started = time.monotonic()
    with pytest.raises(CantonStartupError):
        canton_server_sync(config(facilitator, supported_wait=0.6, supported_attempt_timeout=0.3))
    assert time.monotonic() - started < 3  # bounded by supported_wait, not by the facilitator


@pytest.mark.parametrize(
    "supported",
    [
        {"kinds": [], "extensions": [], "signers": {}},
        {"kinds": [kind_with(network="canton:testnet")["kinds"][0]]},
        {"kinds": [kind_with(x402Version=1)["kinds"][0]]},
        {"kinds": [kind_with(scheme="upto")["kinds"][0]]},
        {"kinds": [kind_with(extra={"synchronizerId": "s"})["kinds"][0]]},
        {"kinds": [kind_with(extra={"feePayer": "f"})["kinds"][0]]},
        {"kinds": [kind_with(extra={"feePayer": "", "synchronizerId": "s"})["kinds"][0]]},
        {"kinds": [kind_with(extra={"feePayer": 5, "synchronizerId": "s"})["kinds"][0]]},
        {"kinds": [{"x402Version": 2, "scheme": "exact", "network": "canton:mainnet"}]},
    ],
    ids=[
        "no-kinds",
        "testnet",
        "v1",
        "upto",
        "no-fee-payer",
        "no-synchronizer",
        "empty-fee-payer",
        "bad-fee-payer",
        "no-extra",
    ],
)
def test_startup_refuses_kinds_that_cannot_build_a_canton_offer(
    facilitator: MockFacilitator, supported: dict[str, Any]
) -> None:
    facilitator.supported = supported
    with pytest.raises(CantonStartupError):
        canton_server_sync(config(facilitator))


def test_a_token_admin_not_listed_by_supported_is_a_startup_error(
    facilitator: MockFacilitator,
) -> None:
    routes = {PAID_ROUTE: Route(pay_to=PAY_TO, price=token(ADMIN, "USDCx", "1"))}
    with pytest.raises(CantonStartupError, match="not in the facilitator's /supported instruments"):
        canton_server_sync(config(facilitator, routes=routes))
    assert facilitator.count("/supported") == 1  # permanent: not retried


def test_a_listed_token_admin_is_accepted(facilitator: MockFacilitator) -> None:
    supported = default_supported()
    supported["kinds"][0]["extra"]["instruments"].append(
        {"instrumentId": {"admin": ADMIN, "id": "USDCx"}}
    )
    facilitator.supported = supported
    routes = {PAID_ROUTE: Route(pay_to=PAY_TO, price=token(ADMIN, "USDCx", "1"))}
    canton_server_sync(config(facilitator, routes=routes)).close()


def test_the_token_admin_check_is_skipped_when_no_instruments_are_listed(
    facilitator: MockFacilitator,
) -> None:
    supported = default_supported()
    del supported["kinds"][0]["extra"]["instruments"]
    facilitator.supported = supported
    routes = {PAID_ROUTE: Route(pay_to=PAY_TO, price=token(ADMIN, "USDCx", "1"))}
    canton_server_sync(config(facilitator, routes=routes)).close()


def test_canton_coin_needs_no_instrument_listing(facilitator: MockFacilitator) -> None:
    supported = default_supported()
    supported["kinds"][0]["extra"]["instruments"] = [
        {"instrumentId": {"admin": ADMIN, "id": "USDCx"}}
    ]
    facilitator.supported = supported
    canton_server_sync(config(facilitator)).close()


def test_bad_config_is_an_error_not_a_crash(facilitator: MockFacilitator) -> None:
    with pytest.raises(CantonConfigError) as raised:
        CantonServer(
            Config(
                routes={"GET /x": Route(pay_to="x", price=cc("0"), memo=" a")},
                facilitator_url=facilitator.url,
            )
        )
    assert len(raised.value.problems) >= 3
    assert facilitator.calls == []  # nothing was contacted


def test_the_sdks_fatal_init_path_never_ends_the_process(
    facilitator: MockFacilitator, monkeypatch: pytest.MonkeyPatch
) -> None:
    """The SDK's adapters call os._exit(1) on a fatal init error. We call initialize()
    ourselves, so the exit is unreachable: prove it with a trap and a forced fatal error."""
    exits: list[int] = []
    monkeypatch.setattr(background_init, "_process_exit", exits.append)
    monkeypatch.setattr(os, "_exit", exits.append)

    def fatal(self: Any) -> None:
        raise RouteConfigurationError([])

    monkeypatch.setattr(x402HTTPResourceServerSync, "initialize", fatal)
    with pytest.raises(CantonStartupError, match="initialization failed"):
        canton_server_sync(config(facilitator, supported_wait=5.0))
    assert exits == []
    assert facilitator.count("/supported") == 1  # a permanent problem is not retried


def test_a_transient_init_error_is_retried_inside_the_same_wait(
    facilitator: MockFacilitator, monkeypatch: pytest.MonkeyPatch
) -> None:
    real = x402HTTPResourceServerSync.initialize
    attempts: list[int] = []

    def flaky(self: Any) -> None:
        attempts.append(1)
        if len(attempts) < 3:
            raise RuntimeError(
                "Failed to initialize: no supported payment kinds loaded from any facilitator."
            )
        real(self)

    monkeypatch.setattr(x402HTTPResourceServerSync, "initialize", flaky)
    server = canton_server_sync(config(facilitator, supported_wait=5.0))
    assert len(attempts) == 3 and server.ready
    server.close()


def test_async_startup_does_not_block_the_event_loop(facilitator: MockFacilitator) -> None:
    facilitator.supported_queue.append(Answer(200, default_supported(), delay=0.4))

    async def run() -> tuple[bool, int]:
        ticks = 0

        async def ticker() -> None:
            nonlocal ticks
            while True:
                await asyncio.sleep(0.02)
                ticks += 1

        task = asyncio.ensure_future(ticker())
        server = await canton_server(config(facilitator, supported_wait=5.0))
        task.cancel()
        server.close()
        return server.ready, ticks

    ready, ticks = asyncio.run(run())
    assert ready and ticks >= 5


def test_before_start_a_paid_route_answers_503_and_never_serves_paid_content(
    facilitator: MockFacilitator,
) -> None:
    server = inproc.build_server(facilitator, start=False)
    with inproc.wsgi_client(server, inproc.wsgi_app("200 OK", b"PAID CONTENT")) as client:
        response = client.get("/paid")
        assert response.status_code == 503 and b"PAID CONTENT" not in response.content
        assert response.headers["retry-after"] == "3"
        assert client.get("/free").status_code == 200  # other routes are untouched
    assert facilitator.calls == []
    server.close()


def test_the_memory_store_warns_at_startup_and_redis_does_not(
    facilitator: MockFacilitator, caplog: pytest.LogCaptureFixture
) -> None:
    CantonServer(config(facilitator)).close()
    assert any("memory store: one process only" in r.getMessage() for r in caplog.records)
    caplog.clear()
    CantonServer(config(facilitator, store=RedisStore(FakeRedis(), "p:"))).close()
    assert not any("memory store" in r.getMessage() for r in caplog.records)


def test_the_facilitator_url_default_and_override(facilitator: MockFacilitator) -> None:
    assert Config(routes={}).facilitator_url == "https://facilitator.ftptech.xyz"
    server = canton_server_sync(
        config(facilitator, facilitator_url=facilitator.url + "/")
    )  # a trailing slash is fine
    assert server.ready
    server.close()


def test_a_merchant_client_is_used_not_replaced(facilitator: MockFacilitator) -> None:
    client = httpx.Client(headers={"X-Merchant": "yes"})
    server = canton_server_sync(config(facilitator, http_client=client))
    server.close()
    assert not client.is_closed  # not ours to close
    client.close()
    assert copy.copy(facilitator.calls[0].headers).get("user-agent") == "ftptech-x402-canton"
    logging.getLogger("x").debug("done")
