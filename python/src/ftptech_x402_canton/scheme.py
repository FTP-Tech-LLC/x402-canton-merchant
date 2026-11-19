"""The mirror scheme plus the one FTP-specific check: what a Canton 402 cannot do without."""

from __future__ import annotations

from collections.abc import Mapping

from x402.interfaces import PaymentFlowConfig
from x402.schemas import Network, SupportedKind

from .mechanisms.canton import ASSET_TRANSFER_METHOD_TRANSFER_FACTORY
from .mechanisms.canton.exact import ExactCantonScheme


class FtpCantonScheme(ExactCantonScheme):
    """The upstream-identical scheme, with a startup check on the facilitator's kind.

    Subclassing keeps the mirror untouched. The SDK finds the optional
    ``validate_facilitator_support`` method by name when the scheme is registered.
    """

    # The FTP layer offers the authorization flow only (settle after the handler): the mirror
    # declares upfront too, which this plugin never offers.
    payment_flows: Mapping[str, PaymentFlowConfig] = {
        ASSET_TRANSFER_METHOD_TRANSFER_FACTORY: {
            "supported": ("authorization",),
            "default": "authorization",
        }
    }

    def validate_facilitator_support(
        self, network: Network, supported_kind: SupportedKind, extensions: list[str]
    ) -> str | None:
        """Make a facilitator that cannot build a Canton 402 a startup error instead of a payment-time one."""
        del network, extensions
        return missing_kind_extra(supported_kind)


def missing_kind_extra(kind: SupportedKind) -> str | None:
    """A problem text when the kind lacks ``extra.feePayer`` or ``extra.synchronizerId``, else None."""
    extra = kind.extra or {}
    for name in ("feePayer", "synchronizerId"):
        value = extra.get(name)
        if not isinstance(value, str) or not value:
            return f"facilitator /supported kind has no extra.{name}"
    return None
