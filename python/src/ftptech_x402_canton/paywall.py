"""The page a browser gets for an unpaid request.

Without it the SDK serves its EVM/USDC paywall: megabytes of script, with the
amount shown 10,000 times too high. This page is a few hundred bytes with the
right Canton amount. The offer itself is in the PAYMENT-REQUIRED header, as for
every other client.
"""

from __future__ import annotations

import html

from x402.http.types import PaywallConfig
from x402.schemas import PaymentRequired

from .mechanisms.canton import CANTON_COIN_DECIMALS


class CantonPaywall:
    """An x402 ``PaywallProvider``."""

    def generate_html(
        self, payment_required: PaymentRequired, config: PaywallConfig | None = None
    ) -> str:
        del config
        amount = "the listed amount"
        if payment_required.accepts:
            accepted = payment_required.accepts[0]
            amount = html.escape(
                f"{display_amount(accepted.amount)} {display_asset(accepted.asset)}"
            )
        return (
            "<!doctype html><meta charset=utf-8><title>Payment required</title>"
            f"<h1>Payment required: {amount}</h1>"
            "<p>This endpoint is paid with the x402 protocol. "
            "Use an x402 client that supports Canton.</p>"
        )


def display_asset(asset: str) -> str:
    return asset.rpartition("::")[
        2
    ]  # the token id (a structured asset is <admin>::<id>); CC has none


def display_amount(atomic: str) -> str:
    """Render an atomic amount (1 unit = 1e-10) as a decimal."""
    if not atomic or not atomic.isascii() or not atomic.isdigit():
        return atomic
    padded = atomic.rjust(CANTON_COIN_DECIMALS + 1, "0")
    whole, fraction = padded[:-CANTON_COIN_DECIMALS], padded[-CANTON_COIN_DECIMALS:].rstrip("0")
    return f"{whole}.{fraction}" if fraction else whole
