"""Canton resource-server (merchant) implementation of the ``exact`` scheme.

An exact port of the TypeScript ``ExactCantonScheme``
(typescript/packages/mechanisms/canton/src/exact/server/scheme.ts). It declares
the payment flows, checks that the merchant's ``asset`` and
``extra.instrumentId`` agree, and merges the facilitator's ``feePayer`` and
``synchronizerId`` (from ``/supported``) into the 402 challenge. Nothing here is
specific to one facilitator or merchant library.
"""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any

from x402.interfaces import PaymentFlowConfig
from x402.schemas import AssetAmount, Network, PaymentRequirements, Price, SupportedKind

from .. import constants as canton


class ExactCantonScheme:
    """Server-side ``exact`` scheme for Canton networks."""

    scheme: str = canton.SCHEME_EXACT
    default_asset_transfer_method: str = canton.ASSET_TRANSFER_METHOD_TRANSFER_FACTORY
    # Settle verifies the payload in full, so it can run before the handler.
    payment_flows: Mapping[str, PaymentFlowConfig] = {
        canton.ASSET_TRANSFER_METHOD_TRANSFER_FACTORY: {
            "supported": ("authorization", "upfront"),
            "default": "authorization",
        }
    }
    # Injected by the facilitator into the 402 ``extra`` (see
    # enhance_payment_requirements), so they are excluded from the
    # requirements-vs-accepted match.
    dynamic_extra_fields: list[str] = ["feePayer", "synchronizerId"]

    def get_asset_decimals(self, asset: str, network: Network) -> int | None:
        """Decimals of a known default asset (Canton Coin is 10), or None."""
        found = canton.find_default_asset(asset, network)
        return found.decimals if found is not None else None

    def parse_price(self, price: Price, network: Network) -> AssetAmount:
        """Accept only an explicit ``AssetAmount`` (``{amount: <atomic>, asset}``).

        A bare money string such as ``"$0.10"`` has no Canton Coin peg and is refused.
        """
        if isinstance(price, AssetAmount):
            fields: dict[str, Any] = {
                "amount": price.amount,
                "asset": price.asset,
                "extra": price.extra,
            }
        elif isinstance(price, dict) and "amount" in price:
            fields = price
        else:
            raise ValueError(
                "The Canton exact scheme requires an explicit AssetAmount price "
                '({ amount: "<atomic units>", asset: "CC" | "<registry symbol>" }); '
                "dollar-string pricing has no Canton Coin peg."
            )
        if not fields.get("asset"):
            raise ValueError(f"Asset must be specified for an AssetAmount on network {network}")
        return AssetAmount(
            amount=fields["amount"], asset=fields["asset"], extra=fields.get("extra") or {}
        )

    def enhance_payment_requirements(
        self,
        requirements: PaymentRequirements,
        supported_kind: SupportedKind,
        extensions: list[str],
    ) -> PaymentRequirements:
        """Merge the facilitator's ``feePayer`` and ``synchronizerId`` into the 402.

        - ``assetTransferMethod`` is set first, so a merchant ``extra`` can still carry it.
        - ``feePayer`` always comes from the supported kind when the kind has one.
        - ``synchronizerId`` comes from the kind only when the merchant did not set it.
        """
        del extensions  # facilitator extension keys are unused here
        extra: dict[str, Any] = {
            "assetTransferMethod": canton.ASSET_TRANSFER_METHOD_TRANSFER_FACTORY,
            **(requirements.extra or {}),
        }
        kind_extra = supported_kind.extra or {}
        if "feePayer" in kind_extra:
            extra["feePayer"] = kind_extra["feePayer"]
        if "synchronizerId" not in extra and "synchronizerId" in kind_extra:
            extra["synchronizerId"] = kind_extra["synchronizerId"]
        enhanced = requirements.model_copy(update={"extra": extra})
        assert_asset_instrument_consistency(enhanced)
        return enhanced


def assert_asset_instrument_consistency(requirements: PaymentRequirements) -> None:
    """A structured ``asset`` of the form ``<admin>::<id>`` must equal ``extra.instrumentId``.

    The facilitator validates against ``instrumentId`` and ignores ``asset``, so a
    silent disagreement would settle the wrong instrument. A symbolic asset such
    as ``CC`` is not cross-checked.

    Raises:
        ValueError: when a structured asset disagrees with ``extra.instrumentId``.
    """
    instrument = canton.instrument_id_from_extra(requirements.extra)
    if instrument is None or "::" not in requirements.asset:
        return
    expected = str(instrument)
    if requirements.asset != expected:
        raise ValueError(
            f'payment requirements asset "{requirements.asset}" disagrees with '
            f'extra.instrumentId ("{expected}"). On Canton the instrumentId is '
            "authoritative: make them consistent."
        )
