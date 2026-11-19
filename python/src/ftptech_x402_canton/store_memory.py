"""The default store: one process only, atomic, TTL-bound and size-bound."""

from __future__ import annotations

import threading
import time
from collections.abc import Callable

from . import forksafe
from .errors import StoreFullError
from .store import Record, delivered_mark

# A delivered payment holds two entries (record and claim) for 26 hours, so the
# default admits about 100,000 paid calls per 26 hours on one instance, around
# 1 MB per 10,000.
DEFAULT_MAX_ENTRIES = 200_000
# What a payment that holds a lock may still add: its record and its claim.
# ``lock`` admits a payment only with room for both, so a payment that was
# admitted can always be recorded and claimed.
LOCK_HEADROOM = 2
# Limits the O(n) scan for expired entries on a full store.
PURGE_INTERVAL_SECONDS = 1.0

_LOCK, _RECORD, _CLAIM = "lock:", "record:", "claimed:"
KEY_LOCK, KEY_RECORD, KEY_CLAIM = _LOCK, _RECORD, _CLAIM


class MemoryStore:
    """In-memory :class:`~ftptech_x402_canton.store.Store` for a single process.

    It forgets everything on restart (a replay inside the payment deadline would
    then be delivered again). It never evicts a live entry. The bound is applied
    when a payment is admitted (``lock``), which keeps room for its record and
    claim; ``put_record`` and ``claim`` are never refused, so a payment that was
    admitted and settled can always be recorded and delivered. A full store
    refuses new payments with ``StoreFullError``, and the gate answers 503 before
    anything is settled.
    """

    # The calls never block on I/O, so the async driver may call them directly.
    non_blocking = True

    def __init__(
        self,
        max_entries: int = DEFAULT_MAX_ENTRIES,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        if max_entries < 1 + LOCK_HEADROOM:
            raise ValueError(f"max_entries must be at least {1 + LOCK_HEADROOM}")
        self._max_entries = max_entries
        self._clock = clock
        self._mutex = threading.Lock()
        self._entries: dict[str, tuple[str, float]] = {}
        self._last_purge = float("-inf")
        forksafe.register(self)

    def reset_after_fork(self) -> None:
        self._mutex = threading.Lock()

    def lock(self, hash: str, token: str, ttl: float) -> bool:
        return self._set_if_absent(_LOCK + hash, token, ttl, admission=True)

    def lock_holder(self, hash: str) -> str | None:
        return self._get(_LOCK + hash)

    def unlock(self, hash: str, token: str) -> None:
        self._delete_if_value(_LOCK + hash, token)

    def record(self, hash: str) -> Record | None:
        value = self._get(_RECORD + hash)
        return None if value is None else Record.from_json(value)

    def put_record(self, hash: str, record: Record, ttl: float) -> None:
        with self._mutex:
            self._insert(_RECORD + hash, record.to_json(), ttl)

    def delete_record(self, hash: str) -> None:
        with self._mutex:
            self._entries.pop(_RECORD + hash, None)

    def claim(self, update_id: str, token: str, ttl: float) -> bool:
        return self._set_if_absent(_CLAIM + update_id, token, ttl, admission=False)

    def claimant(self, update_id: str) -> str | None:
        return self._get(_CLAIM + update_id)

    def unclaim(self, update_id: str, token: str) -> None:
        self._delete_if_value(_CLAIM + update_id, token)

    def mark_delivered(self, update_id: str, token: str, ttl: float) -> None:
        with self._mutex:
            self._insert(_CLAIM + update_id, delivered_mark(token), ttl)

    def __len__(self) -> int:
        with self._mutex:
            return len(self._entries)

    def _get(self, key: str) -> str | None:
        with self._mutex:
            entry = self._entries.get(key)
            if entry is None or self._clock() >= entry[1]:
                self._entries.pop(key, None)
                return None
            return entry[0]

    def _delete_if_value(self, key: str, value: str) -> None:
        with self._mutex:
            entry = self._entries.get(key)
            if entry is not None and entry[0] == value:
                del self._entries[key]

    def _set_if_absent(self, key: str, value: str, ttl: float, *, admission: bool) -> bool:
        with self._mutex:
            entry = self._entries.get(key)
            if entry is not None and self._clock() < entry[1]:
                return False
            if (
                admission
                and not self._has_live_record(key)
                and not self._has_room(1 + LOCK_HEADROOM)
            ):
                raise StoreFullError("memory store is full of live entries")
            self._insert(key, value, ttl)
            return True

    def _has_live_record(self, lock_key: str) -> bool:
        """Whether the payment behind a lock key already has a live record: it was admitted
        before, and its retry adds at most a lock and one claim, so it needs no admission room."""
        entry = self._entries.get(_RECORD + lock_key[len(_LOCK) :])
        return entry is not None and self._clock() < entry[1]

    def _insert(self, key: str, value: str, ttl: float) -> None:
        self._entries[key] = (value, self._clock() + ttl)

    def _has_room(self, extra: int) -> bool:
        """Whether ``extra`` more entries fit; on a full store purge expired entries
        (one O(n) scan, at most once per interval) and check again."""
        if len(self._entries) + extra <= self._max_entries:
            return True
        now = self._clock()
        if now - self._last_purge >= PURGE_INTERVAL_SECONDS:
            self._last_purge = now
            for key in [k for k, (_, expires) in self._entries.items() if now >= expires]:
                del self._entries[key]
        return len(self._entries) + extra <= self._max_entries
