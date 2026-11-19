"""Constants and helpers of the Canton ``exact`` x402 scheme.

Mirrors the TypeScript package ``@x402/canton`` (constants.ts, types.ts,
defaultAssets.ts) so that a later move into the official SDK only changes the
import path. Nothing in this package is specific to one facilitator or merchant
library.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Final

SCHEME_EXACT: Final = "exact"

CANTON_MAINNET_CAIP2: Final = "canton:mainnet"
CANTON_TESTNET_CAIP2: Final = "canton:testnet"
CANTON_DEVNET_CAIP2: Final = "canton:devnet"
#: CAIP family pattern used to group signers in a supported response. It is not
#: a valid network for scheme registration.
CAIP_FAMILY: Final = "canton:*"
CANTON_NETWORKS: Final = (CANTON_MAINNET_CAIP2, CANTON_TESTNET_CAIP2, CANTON_DEVNET_CAIP2)

#: The only on-ledger settlement method: a token-standard TransferFactory_Transfer.
ASSET_TRANSFER_METHOD_TRANSFER_FACTORY: Final = "transfer-factory"

#: Canton Coin symbol as advertised in ``asset``.
CANTON_COIN_SYMBOL: Final = "CC"
#: Canton Coin ledger precision: 1 CC = 1e10 atomic units.
CANTON_COIN_DECIMALS: Final = 10

# Error codes of the exact scheme, as in the TypeScript types.ts. They appear in
# VerifyResponse.invalidReason and SettleResponse.errorReason.
ERR_AMOUNT_MISMATCH: Final = "invalid_exact_canton_amount_mismatch"
ERR_EXECUTE_BEFORE_TOO_FAR: Final = "invalid_exact_canton_execute_before_too_far"
ERR_EXECUTE_FAILED: Final = "invalid_exact_canton_execute_failed"
ERR_EXPIRED: Final = "invalid_exact_canton_expired"
ERR_FEE_PAYER_MISMATCH: Final = "invalid_exact_canton_fee_payer_mismatch"
ERR_INSTRUMENT_ID_MISMATCH: Final = "invalid_exact_canton_instrument_id_mismatch"
ERR_INSUFFICIENT_INPUTS: Final = "invalid_exact_canton_insufficient_inputs"
ERR_MALFORMED_PAYLOAD: Final = "invalid_exact_canton_malformed_payload"
ERR_MEMO_MISMATCH: Final = "invalid_exact_canton_memo_mismatch"
ERR_MERCHANT_MISMATCH: Final = "invalid_exact_canton_merchant_mismatch"
ERR_MERCHANT_NOT_REGISTERED: Final = "invalid_exact_canton_merchant_not_registered"
ERR_MISSING_PROOF: Final = "invalid_exact_canton_missing_proof"
ERR_PREAPPROVAL_MISSING: Final = "invalid_exact_canton_preapproval_missing"
ERR_SELF_PAYMENT: Final = "invalid_exact_canton_self_payment"
ERR_SIGNATURE_INVALID: Final = "invalid_exact_canton_signature_invalid"
ERR_UNEXPECTED_LEDGER_ERROR: Final = "unexpected_canton_ledger_error"
ERR_INVALID_NETWORK: Final = "invalid_network"
ERR_SETTLEMENT_PENDING: Final = "settlement_pending"
ERR_UNSUPPORTED_SCHEME: Final = "unsupported_scheme"


@dataclass(frozen=True)
class InstrumentId:
    """A Canton instrument: the registrar (admin) party and the instrument id.

    Canton Coin is ``InstrumentId(DSO party, "Amulet")``.
    """

    admin: str
    id: str

    def __str__(self) -> str:
        """The structured asset form ``<admin>::<id>``."""
        return f"{self.admin}::{self.id}"


def instrument_id_from_extra(extra: dict[str, Any] | None) -> InstrumentId | None:
    """Read ``extra["instrumentId"]``; None when absent or not ``{admin, id}`` strings."""
    if not extra:
        return None
    raw = extra.get("instrumentId")
    if isinstance(raw, InstrumentId):
        return raw
    if isinstance(raw, dict):
        admin, ident = raw.get("admin"), raw.get("id")
        if isinstance(admin, str) and isinstance(ident, str):
            return InstrumentId(admin, ident)
    return None


def is_canton_network(network: str) -> bool:
    """True for ``canton:mainnet``, ``canton:testnet`` and ``canton:devnet``."""
    return network in CANTON_NETWORKS


@dataclass(frozen=True)
class DefaultAsset:
    """An asset the scheme knows without configuration."""

    asset: str
    symbol: str
    decimals: int


#: The Canton Coin default asset. Its identity is symbolic.
CANTON_COIN_ASSET: Final = DefaultAsset(
    asset=CANTON_COIN_SYMBOL, symbol=CANTON_COIN_SYMBOL, decimals=CANTON_COIN_DECIMALS
)


def find_default_asset(asset: str, network: str) -> DefaultAsset | None:
    """The default asset for a symbol on a Canton network, or None.

    Canton Coin has no USD peg, so only ``CC`` and ``canton-coin`` are known.
    """
    if not is_canton_network(network):
        return None
    if asset in (CANTON_COIN_SYMBOL, "canton-coin"):
        return CANTON_COIN_ASSET
    return None
