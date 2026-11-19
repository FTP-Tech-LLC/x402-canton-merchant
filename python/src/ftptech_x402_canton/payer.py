"""What a paid handler can ask about its request."""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any

#: The key under which the drivers put the proven payer into the ASGI scope or the WSGI environ.
PAYER_KEY = "ftptech_x402_canton.payer"
RETRY_KEY = "ftptech_x402_canton.retry"


def _carrier(request: Any) -> Mapping[str, Any] | None:
    """The ASGI scope or WSGI environ behind a Starlette or Flask request."""
    if request is None:
        try:
            from flask import request as flask_request
        except ImportError:
            return None
        try:
            return dict(flask_request.environ)
        except RuntimeError:  # outside a request context
            return None
    if isinstance(request, Mapping):
        return request
    for name in ("scope", "environ"):
        carrier = getattr(request, name, None)
        if isinstance(carrier, Mapping):
            return carrier
    return None


def payer_from(request: Any = None) -> str | None:
    """The payer the facilitator proved in /verify, for use inside a paid handler.

    Pass the Starlette/FastAPI ``Request`` or the Flask ``request`` (Flask also
    works with no argument). On a retry that skips /verify it is the payer
    recorded when the payment was attempted. None outside a paid request, and when
    no payer is known.

    A handler that answers 429 or any other 4xx/5xx for an over-limit payer is not
    settled, so the payer is not charged. The exception is a retry of a payment
    that may already have moved (see :func:`is_retry`): there the gate does not
    pass a 4xx or 5xx through, it answers 503 "do NOT pay again; retry the same payment".
    """
    carrier = _carrier(request)
    value = carrier.get(PAYER_KEY) if carrier is not None else None
    return value if isinstance(value, str) and value else None


def is_retry(request: Any = None) -> bool:
    """True when the request is the retry of a payment this merchant already attempted
    or settled but did not deliver. Such a payment may already have moved, so a
    limiter should not refuse it."""
    carrier = _carrier(request)
    return bool(carrier.get(RETRY_KEY)) if carrier is not None else False
