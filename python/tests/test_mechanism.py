"""The mirror scheme: a port of typescript/packages/mechanisms/canton/test/unit/server.test.ts,
plus the cases the Go port added. Nothing FTP-specific is tested or imported here."""

from __future__ import annotations

import inspect
from typing import Any, cast

import pytest
from x402.interfaces import SchemeNetworkServer
from x402.payment_flow import resolve_payment_flow
from x402.schemas import AssetAmount, PaymentRequirements, Price, SupportedKind

from ftptech_x402_canton.mechanisms import canton
from ftptech_x402_canton.mechanisms.canton import InstrumentId
from ftptech_x402_canton.mechanisms.canton import exact as exact_package
from ftptech_x402_canton.mechanisms.canton.exact import (
    ExactCantonScheme,
    assert_asset_instrument_consistency,
)
from ftptech_x402_canton.mechanisms.canton.exact import server as exact_server

NETWORK = "canton:mainnet"
DSO = "DSO::1220" + "aa" * 32
FAC = "facilitator::1220" + "ff" * 32


def base_requirements() -> PaymentRequirements:
    return PaymentRequirements(
        scheme="exact",
        network=NETWORK,
        amount="100000000",
        asset="CC",
        pay_to="merchant::1220" + "bb" * 32,
        max_timeout_seconds=60,
        extra={"instrumentId": {"admin": DSO, "id": "Amulet"}},
    )


def supported(**extra: Any) -> SupportedKind:
    return SupportedKind(
        x402_version=2,
        scheme="exact",
        network=NETWORK,
        extra={"feePayer": FAC, "synchronizerId": "global-domain::1220test", **extra},
    )


scheme = ExactCantonScheme()


def parse(price: Any, network: str) -> AssetAmount:
    """A price as a merchant may pass it: the SDK's ``Price`` type does not list a plain mapping."""
    return scheme.parse_price(cast(Price, price), network)


def test_declares_the_transfer_factory_authorization_and_upfront_flows() -> None:
    assert scheme.scheme == "exact"
    assert scheme.default_asset_transfer_method == "transfer-factory"
    flows = scheme.payment_flows["transfer-factory"]
    assert flows["default"] == "authorization"
    assert list(flows["supported"]) == ["authorization", "upfront"]


def test_resolves_an_upfront_route_through_the_sdks_payment_flow_table() -> None:
    requirements = base_requirements().model_copy(update={"extra": {"paymentFlow": "upfront"}})
    resolved = resolve_payment_flow(scheme, requirements)
    assert (resolved.asset_transfer_method, resolved.payment_flow) == (
        "transfer-factory",
        "upfront",
    )


def test_dynamic_extra_fields_are_exactly_fee_payer_and_synchronizer() -> None:
    assert scheme.dynamic_extra_fields == ["feePayer", "synchronizerId"]


def test_is_a_scheme_network_server() -> None:
    server: SchemeNetworkServer = scheme  # structural: mypy checks the protocol
    assert server.scheme == "exact"


def test_get_asset_decimals_is_10_for_canton_coin() -> None:
    assert scheme.get_asset_decimals("CC", NETWORK) == 10
    assert scheme.get_asset_decimals("canton-coin", NETWORK) == 10
    assert scheme.get_asset_decimals("SOMETHING", NETWORK) is None
    assert scheme.get_asset_decimals("CC", "eip155:1") is None


def test_parse_price_accepts_an_explicit_asset_amount() -> None:
    assert parse({"amount": "100000000", "asset": "CC"}, NETWORK) == AssetAmount(
        amount="100000000", asset="CC", extra={}
    )
    typed = parse(AssetAmount(amount="5", asset="CC", extra={"k": 1}), NETWORK)
    assert typed.extra == {"k": 1}


@pytest.mark.parametrize("money", ["$0.10", 0.1, 5, "1.5", {"price": "x"}])
def test_parse_price_rejects_money_values(money: Any) -> None:
    with pytest.raises(ValueError, match="explicit AssetAmount"):
        parse(money, NETWORK)


def test_parse_price_rejects_a_missing_asset_and_a_bad_amount_type() -> None:
    with pytest.raises(ValueError, match="Asset must be specified"):
        parse({"amount": "1"}, NETWORK)
    with pytest.raises(ValueError):
        parse({"amount": 100, "asset": "CC"}, NETWORK)


def test_enhance_merges_fee_payer_synchronizer_and_method() -> None:
    out = scheme.enhance_payment_requirements(base_requirements(), supported(), [])
    assert out.extra["feePayer"] == FAC
    assert out.extra["synchronizerId"] == "global-domain::1220test"
    assert out.extra["assetTransferMethod"] == "transfer-factory"
    assert out.extra["instrumentId"] == {"admin": DSO, "id": "Amulet"}


def test_enhance_fee_payer_always_comes_from_the_supported_kind() -> None:
    requirements = base_requirements()
    requirements.extra["feePayer"] = "merchant-chosen::1220" + "11" * 32
    assert (
        scheme.enhance_payment_requirements(requirements, supported(), []).extra["feePayer"] == FAC
    )


def test_enhance_keeps_a_merchant_synchronizer_id() -> None:
    requirements = base_requirements()
    requirements.extra["synchronizerId"] = "pinned::1220" + "22" * 32
    out = scheme.enhance_payment_requirements(requirements, supported(), [])
    assert out.extra["synchronizerId"] == "pinned::1220" + "22" * 32


def test_enhance_sets_the_method_first_so_a_merchant_extra_wins() -> None:
    requirements = base_requirements()
    requirements.extra["assetTransferMethod"] = "custom"
    assert (
        scheme.enhance_payment_requirements(requirements, supported(), []).extra[
            "assetTransferMethod"
        ]
        == "custom"
    )


def test_enhance_does_not_mutate_its_input() -> None:
    requirements = base_requirements()
    scheme.enhance_payment_requirements(requirements, supported(), [])
    assert requirements.extra == {"instrumentId": {"admin": DSO, "id": "Amulet"}}


def test_enhance_without_kind_extra_leaves_the_fields_absent() -> None:
    kind = SupportedKind(x402_version=2, scheme="exact", network=NETWORK)
    out = scheme.enhance_payment_requirements(base_requirements(), kind, [])
    assert "feePayer" not in out.extra and "synchronizerId" not in out.extra
    assert out.extra["assetTransferMethod"] == "transfer-factory"


def test_enhance_rejects_an_asset_that_disagrees_with_the_instrument() -> None:
    requirements = base_requirements().model_copy(update={"asset": "wrong::Amulet"})
    with pytest.raises(ValueError, match="disagrees"):
        scheme.enhance_payment_requirements(requirements, supported(), [])


def test_consistency_is_a_no_op_for_a_symbolic_asset() -> None:
    assert_asset_instrument_consistency(base_requirements())


def test_consistency_is_a_no_op_without_an_instrument() -> None:
    assert_asset_instrument_consistency(
        base_requirements().model_copy(update={"asset": "x::y", "extra": {}})
    )
    assert_asset_instrument_consistency(
        base_requirements().model_copy(
            update={"asset": "x::y", "extra": {"instrumentId": "not-an-object"}}
        )
    )


def test_consistency_passes_when_a_structured_asset_matches() -> None:
    assert_asset_instrument_consistency(
        base_requirements().model_copy(update={"asset": f"{DSO}::Amulet"})
    )


def test_consistency_throws_when_a_structured_asset_disagrees() -> None:
    with pytest.raises(ValueError, match="disagrees"):
        assert_asset_instrument_consistency(
            base_requirements().model_copy(update={"asset": "wrong::Amulet"})
        )


def test_the_mirror_has_nothing_ftp_specific() -> None:
    """A later upstream move must only change the import path."""
    for module in (exact_server, exact_package, canton):
        source = inspect.getsource(module).lower()
        assert "ftp" not in source and "facilitator.ftptech" not in source
    assert not hasattr(scheme, "validate_facilitator_support")
    imports = [
        line
        for line in inspect.getsource(exact_server).splitlines()
        if line.startswith(("from ", "import "))
    ]
    assert all(
        "ftptech_x402_canton." not in line or "mechanisms" in line or line.startswith("from ..")
        for line in imports
    )


# -- constants and helpers (constants.ts, types.ts, defaultAssets.ts) ----------------


def test_constants_have_the_upstream_names_and_values() -> None:
    assert canton.SCHEME_EXACT == "exact"
    assert canton.CANTON_MAINNET_CAIP2 == "canton:mainnet"
    assert canton.CANTON_TESTNET_CAIP2 == "canton:testnet"
    assert canton.CANTON_DEVNET_CAIP2 == "canton:devnet"
    assert canton.CAIP_FAMILY == "canton:*"
    assert canton.ASSET_TRANSFER_METHOD_TRANSFER_FACTORY == "transfer-factory"
    assert canton.CANTON_COIN_SYMBOL == "CC" and canton.CANTON_COIN_DECIMALS == 10
    assert canton.ERR_SETTLEMENT_PENDING == "settlement_pending"
    assert canton.ERR_EXPIRED == "invalid_exact_canton_expired"


def test_is_canton_network() -> None:
    assert all(
        canton.is_canton_network(n) for n in ("canton:mainnet", "canton:testnet", "canton:devnet")
    )
    assert not any(
        canton.is_canton_network(n)
        for n in ("canton:*", "canton:", "eip155:1", "", "Canton:mainnet")
    )


def test_find_default_asset() -> None:
    found = canton.find_default_asset("CC", "canton:mainnet")
    assert found is not None and found.decimals == 10 and found.symbol == "CC"
    assert canton.find_default_asset("canton-coin", "canton:devnet") is not None
    assert canton.find_default_asset("USDC", "canton:mainnet") is None
    assert canton.find_default_asset("CC", "eip155:1") is None


def test_instrument_id_forms() -> None:
    assert str(InstrumentId("a", "b")) == "a::b"
    assert canton.instrument_id_from_extra(
        {"instrumentId": {"admin": "a", "id": "b"}}
    ) == InstrumentId("a", "b")
    assert canton.instrument_id_from_extra(
        {"instrumentId": InstrumentId("a", "b")}
    ) == InstrumentId("a", "b")
    assert canton.instrument_id_from_extra({"instrumentId": {"admin": "a"}}) is None
    assert canton.instrument_id_from_extra({"instrumentId": {"admin": 1, "id": 2}}) is None
    assert (
        canton.instrument_id_from_extra({}) is None
        and canton.instrument_id_from_extra(None) is None
    )
