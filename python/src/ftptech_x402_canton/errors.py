"""Errors the plugin raises. It never exits the process: every startup problem is an exception."""

from __future__ import annotations


class CantonConfigError(ValueError):
    """The configuration is invalid. ``problems`` lists every problem found, in one pass."""

    def __init__(self, problems: list[str]) -> None:
        self.problems = list(problems)
        super().__init__("invalid canton config: " + "; ".join(self.problems))


class CantonStartupError(RuntimeError):
    """The facilitator could not be reached or does not offer what a Canton 402 needs."""


class CantonNotReadyError(RuntimeError):
    """The server was used before ``start()`` finished."""


class StoreFullError(Exception):
    """The memory store is full of live entries: a new payment is refused before anything is settled."""
