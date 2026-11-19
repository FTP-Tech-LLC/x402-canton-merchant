"""Rule 2 (routes), prices, config defaults."""

from __future__ import annotations

from typing import Any

import pytest

from ftptech_x402_canton import CantonConfigError, Config, Route, atomic, cc, token
from ftptech_x402_canton.config import check_routes, resolve, sdk_routes
from ftptech_x402_canton.mechanisms.canton import InstrumentId
from ftptech_x402_canton.price import DSO_MAINNET_PARTY
from tests.support import PAY_TO

ADMIN = "usdcx-admin::1220" + "dd" * 32


def route(**kwargs: Any) -> Route:
    return Route(**{"pay_to": PAY_TO, "price": cc("0.01"), **kwargs})


@pytest.mark.parametrize(
    "key",
    [
        "/paid", "GET", "get /paid", "HEAD /paid", "OPTIONS /paid", "GET  /paid", "GET /paid ", " GET /paid",
        "GET paid", "GET /pa id", "GET /paid/", "GET /a//b", "GET /a/./b", "GET /a/../b", "GET /*", "GET /a/*",
        "GET /:id", "GET /a/:id", "GET /[id]", "GET /a?x=1", "GET /a#f", "GET /%70aid", "GET /ü", "GET /a\\b",
        "GET /a\tb", "GET /.", "GET /..", "GET /a/.", "", "GET\t/paid",
    ],
)  # fmt: skip
def test_route_keys_must_be_method_and_a_literal_path(key: str) -> None:
    with pytest.raises(CantonConfigError):
        resolve(Config(routes={key: route()}))


@pytest.mark.parametrize(
    "key",
    [
        "GET /",
        "GET /paid",
        "POST /v1/data",
        "PUT /a-b_c.d",
        "PATCH /a/b/c",
        "DELETE /x",
        "GET /a.b",
        "GET /~x",
        "GET /a+b",
        "GET /a,b",
    ],
)
def test_valid_route_keys(key: str) -> None:
    assert len(resolve(Config(routes={key: route()})).routes) == 1


def test_routes_refuse_paths_that_differ_only_by_case() -> None:
    routes = {"GET /Paid": route(), "GET /paid": route()}
    with pytest.raises(CantonConfigError, match="duplicates"):
        resolve(Config(routes=routes))
    ok = {"GET /paid": route(), "POST /paid": route()}  # another method is another route
    assert len(resolve(Config(routes=ok)).routes) == 2


def test_empty_routes_is_an_error() -> None:
    with pytest.raises(CantonConfigError, match="routes is empty"):
        resolve(Config(routes={}))


@pytest.mark.parametrize(
    ("field", "value", "fragment"),
    [
        ("pay_to", "merchant", "pay_to"),
        ("pay_to", "merchant::1220" + "b" * 63, "pay_to"),
        ("pay_to", "merchant::1220" + "B" * 64, "pay_to"),
        ("pay_to", "merchant::1221" + "b" * 64, "pay_to"),
        ("pay_to", "::1220" + "b" * 64, "pay_to"),
        ("pay_to", "", "pay_to"),
        ("execute_before_seconds", 0, "1..600"),
        ("execute_before_seconds", 601, "1..600"),
        ("execute_before_seconds", -5, "1..600"),
        ("execute_before_seconds", True, "1..600"),
        ("execute_before_seconds", 1.5, "1..600"),
        ("memo", " lead", "whitespace"),
        ("memo", "trail ", "whitespace"),
        ("memo", "x" * 257, "256 bytes"),
        ("memo", "é" * 129, "256 bytes"),  # 258 UTF-8 bytes
        ("memo", "\ud800", "UTF-8"),
        ("price", cc("0"), "greater than zero"),
        ("price", cc("1.12345678901"), "fractional"),
        ("price", cc("-1"), "positive decimal"),
        ("price", cc("1e3"), "positive decimal"),
        ("price", cc(""), "positive decimal"),
        ("price", "0.01", "price is not set"),
    ],
)  # fmt: skip
def test_route_field_validation(field: str, value: Any, fragment: str) -> None:
    with pytest.raises(CantonConfigError, match=fragment):
        resolve(Config(routes={"GET /x": route(**{field: value})}))


@pytest.mark.parametrize(
    "fields",
    [{"execute_before_seconds": 1}, {"execute_before_seconds": 600}, {"memo": "x" * 256}, {"memo": "é" * 128},
     {"memo": ""}, {"memo": "a b"}, {"execute_before_seconds": None}, {"description": "d", "mime_type": "text/plain"}],
)  # fmt: skip
def test_route_field_boundaries_are_accepted(fields: dict[str, Any]) -> None:
    assert len(resolve(Config(routes={"GET /x": route(**fields)})).routes) == 1


def test_route_problems_are_reported_together() -> None:
    routes = {
        "GET /a": route(pay_to="bad"),
        "GET /b": route(execute_before_seconds=0),
        "nope": route(),
        "GET /c": route(memo=" x"),
    }
    with pytest.raises(CantonConfigError) as raised:
        resolve(Config(routes=routes, facilitator_url="ftp://x", settle_calls=0))
    problems = raised.value.problems
    assert len(problems) >= 6
    text = "\n".join(problems)
    for fragment in (
        "'GET /a'",
        "'GET /b'",
        "'nope'",
        "'GET /c'",
        "facilitator_url",
        "settle_calls",
    ):
        assert fragment in text


def test_price_conversions() -> None:
    assert cc("0.01").atomic == "100000000"
    assert cc("1").atomic == "10000000000"
    assert cc("1.5").atomic == "15000000000"
    assert cc("0.0000000001").atomic == "1"
    assert cc("00.5").atomic == "5000000000"
    assert cc("123456789.1234567891").atomic == "1234567891234567891"
    assert cc("0.01").is_canton_coin and cc("0.01").asset == "CC"
    usdcx = token(ADMIN, "USDCx", "2.5")
    assert (
        usdcx.atomic == "25000000000"
        and usdcx.asset == f"{ADMIN}::USDCx"
        and not usdcx.is_canton_coin
    )
    assert token(DSO_MAINNET_PARTY, "Amulet", "1").is_canton_coin
    assert atomic("100", InstrumentId(ADMIN, "USDCx")).atomic == "100"
    assert atomic("9" * 38, InstrumentId(ADMIN, "x")).problem() is None


@pytest.mark.parametrize(
    "amount", ["0", "01", "-1", "1.5", "", " 1", "1 ", "9" * 39, "1e5", "abc", "١٢٣"]
)
def test_atomic_refuses_bad_amounts(amount: str) -> None:
    assert atomic(amount, InstrumentId(ADMIN, "x")).problem() is not None


def test_a_bad_price_is_reported_by_the_config_check_never_by_a_raise_at_the_call_site() -> None:
    price = cc("abc")  # does not raise
    assert price.problem() is not None
    with pytest.raises(CantonConfigError):
        resolve(Config(routes={"GET /x": Route(pay_to=PAY_TO, price=price)}))
    assert atomic("1", InstrumentId("", "x")).problem() == "price instrument needs admin and id"


def test_config_defaults() -> None:
    resolved = resolve(
        Config(
            routes={
                "GET /a": route(execute_before_seconds=30),
                "GET /b": route(execute_before_seconds=200),
            }
        )
    )
    config = resolved.config
    assert resolved.facilitator_url == "https://facilitator.ftptech.xyz"
    assert resolved.timeout == 200.0  # the largest route lifetime
    assert (config.verify_timeout, config.settle_timeout, config.settle_calls) == (30.0, 65.0, 4)
    assert (config.supported_wait, config.retry_after_cap, config.settle_on_redirect) == (
        60.0,
        5.0,
        False,
    )
    assert tuple(config.settle_backoff) == (1.0, 2.0, 4.0)
    assert resolve(Config(routes={"GET /a": route()})).timeout == 120.0


@pytest.mark.parametrize(
    "field",
    [
        "timeout",
        "verify_timeout",
        "settle_timeout",
        "supported_wait",
        "retry_after_cap",
        "settle_calls",
        "worker_threads",
    ],
)
@pytest.mark.parametrize("value", [0, -1, -0.5, float("nan"), float("inf"), "5", True, None])
def test_config_refuses_bad_numbers(field: str, value: Any) -> None:
    if value is None and field == "timeout":
        return  # None means "default"
    with pytest.raises(CantonConfigError, match=field):
        resolve(Config(routes={"GET /a": route()}, **{field: value}))


def test_the_sdk_route_table_never_carries_a_payment_flow() -> None:
    checked, problems = check_routes(
        {
            "GET /a": route(memo="m", description="d", mime_type="text/plain"),
            "POST /b": route(price=token(ADMIN, "USDCx", "1")),
        }
    )
    assert not problems
    table = sdk_routes(checked)
    assert set(table) == {"GET /a", "POST /b"}
    for entry in table.values():
        accepts = entry["accepts"]
        assert accepts["scheme"] == "exact" and accepts["network"] == "canton:mainnet"
        assert "paymentFlow" not in accepts["price"]["extra"] and "paymentFlow" not in accepts.get(
            "extra", {}
        )
        assert (
            accepts["maxTimeoutSeconds"] == accepts["price"]["extra"]["executeBeforeSeconds"] == 120
        )
    a = table["GET /a"]
    assert a["accepts"]["price"] == {
        "amount": "100000000", "asset": "CC",
        "extra": {"instrumentId": {"admin": DSO_MAINNET_PARTY, "id": "Amulet"}, "executeBeforeSeconds": 120, "memo": "m"},
    }  # fmt: skip
    assert a["description"] == "d" and a["mimeType"] == "text/plain"
    assert table["POST /b"]["accepts"]["price"]["asset"] == f"{ADMIN}::USDCx"
    assert "memo" not in table["POST /b"]["accepts"]["price"]["extra"]
