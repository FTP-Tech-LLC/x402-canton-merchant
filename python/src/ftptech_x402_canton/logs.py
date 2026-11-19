"""Structured logging helper. The payment header, the signature and the prepared transaction are never logged."""

from __future__ import annotations

import logging
from typing import Any


def event(logger: logging.Logger, level: int, message: str, **fields: Any) -> None:
    """Log ``message`` with ``fields`` both in the text (``key=value``) and in ``record.canton``.

    Callers pass identifiers only (canonical hash, updateId, status codes), never
    request headers or payload bytes.
    """
    if not logger.isEnabledFor(level):
        return
    text = message
    if fields:
        text += " " + " ".join(f"{key}={value!r}" for key, value in sorted(fields.items()))
    logger.log(level, text, extra={"canton": fields})
