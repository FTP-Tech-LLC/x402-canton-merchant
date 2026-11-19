"""Prices: Canton Coin, registry tokens, or an exact atomic amount."""

from __future__ import annotations

import re
from dataclasses import dataclass

from .mechanisms.canton import CANTON_COIN_DECIMALS, CANTON_COIN_SYMBOL, InstrumentId

#: Canton Coin registrar (DSO) party on MainNet.
DSO_MAINNET_PARTY = "DSO::1220b1431ef217342db44d516bb9befde802be7d8899637d290895fa58880f19accc"
#: Canton Coin instrument id.
CANTON_COIN_INSTRUMENT_ID = "Amulet"

# A Daml Numeric(38, 10) holds at most 38 digits in total, so 38 atomic digits is
# the largest valid amount.
MAX_ATOMIC_DIGITS = 38

_ATOMIC_RE = re.compile(r"[1-9][0-9]*")
_DECIMAL_RE = re.compile(r"[0-9]+(\.[0-9]{1,10})?")


@dataclass(frozen=True)
class Price:
    """What one call of a paid route costs: an atomic amount and its instrument.

    Build it with :func:`cc`, :func:`token` or :func:`atomic`. A price that failed
    to parse is reported by the server's config check (``CantonConfigError``),
    never at the call site.
    """

    atomic: str
    instrument: InstrumentId
    error: str | None = None

    def problem(self) -> str | None:
        """Why this price is unusable, or None."""
        if self.error is not None:
            return self.error
        if not self.atomic:
            return "price is not set; use cc(), token() or atomic()"
        if not self.instrument.admin or not self.instrument.id:
            return "price instrument needs admin and id"
        return None

    @property
    def is_canton_coin(self) -> bool:
        return self.instrument == InstrumentId(DSO_MAINNET_PARTY, CANTON_COIN_INSTRUMENT_ID)

    @property
    def asset(self) -> str:
        """The 402 ``asset``: ``CC`` for Canton Coin, ``<admin>::<id>`` otherwise."""
        return CANTON_COIN_SYMBOL if self.is_canton_coin else str(self.instrument)


def cc(decimal: str) -> Price:
    """Price a route in Canton Coin, for example ``cc("0.01")``.

    The decimal has at most 10 fractional digits and must be positive.
    """
    return token(DSO_MAINNET_PARTY, CANTON_COIN_INSTRUMENT_ID, decimal)


def token(admin: str, instrument_id: str, decimal: str) -> Price:
    """Price a route in a registry token, for example ``token(admin, "USDCx", "1.5")``.

    The admin must be a party the facilitator supports; startup checks it against ``/supported``.
    """
    instrument = InstrumentId(admin, instrument_id)
    try:
        return Price(_decimal_to_atomic(decimal), instrument)
    except ValueError as error:
        return Price("", instrument, str(error))


def atomic(amount: str, instrument: InstrumentId) -> Price:
    """Price a route with an exact atomic amount (1 unit = 1e-10), matching ``^[1-9][0-9]*$``."""
    if (
        not isinstance(amount, str)
        or _ATOMIC_RE.fullmatch(amount) is None
        or len(amount) > MAX_ATOMIC_DIGITS
    ):
        return Price(
            "",
            instrument,
            f"atomic amount {amount!r} must match ^[1-9][0-9]*$ and have at most "
            f"{MAX_ATOMIC_DIGITS} digits",
        )
    return Price(amount, instrument)


def _decimal_to_atomic(decimal: str) -> str:
    if not isinstance(decimal, str) or _DECIMAL_RE.fullmatch(decimal) is None:
        raise ValueError(
            f"price {decimal!r} must be a positive decimal with at most "
            f"{CANTON_COIN_DECIMALS} fractional digits"
        )
    whole, _, fraction = decimal.partition(".")
    digits = (whole + fraction.ljust(CANTON_COIN_DECIMALS, "0")).lstrip("0")
    if not digits:
        raise ValueError(f"price {decimal!r} must be greater than zero")
    if len(digits) > MAX_ATOMIC_DIGITS:
        raise ValueError(
            f"price {decimal!r} is too large: at most {MAX_ATOMIC_DIGITS} atomic digits"
        )
    return digits
