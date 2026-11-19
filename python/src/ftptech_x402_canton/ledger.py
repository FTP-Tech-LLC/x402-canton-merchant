"""The gate's view of the store.

Locks and claims that survive an ambiguous reply, records that are never
overwritten by a different payment, and an in-process fallback so a failed store
does not lose the fact that a payment was attempted or settled. What a store
failure leaves behind in this process (a claim or lock that could not be
released, a record only the fallback holds, an attempt whose clear failed) is
retried by :meth:`Ledger.reconcile_once`, which a background reconciler calls
until each item succeeds or its TTL ends.
"""

from __future__ import annotations

import logging
import threading
import time
from collections.abc import Callable
from dataclasses import dataclass
from enum import Enum
from functools import partial

from . import forksafe
from .errors import StoreFullError
from .identity import new_token
from .logs import event
from .store import (
    RECORD_TTL_SECONDS,
    STATE_SETTLED,
    Record,
    Store,
    is_delivered_mark,
)

MAX_FALLBACK_RECORDS = 4096
#: Bounds the claims and locks remembered for release.
MAX_PENDING_RELEASES = 16384
#: How often a release or a record that is still pending is logged.
RECONCILE_LOG_EVERY_SECONDS = 30.0
#: How often a lock or claim whose reply was lost is tried.
AMBIGUOUS_RETRIES = 2
#: The reconciler's hold on one payment while it writes a record.
RECONCILE_LOCK_TTL_SECONDS = 30.0


class RecordConflictError(Exception):
    """A different payment already owns the hash: the gate refuses to overwrite it and fails closed."""


class ClaimHeldError(Exception):
    """A claim of another request is on the updateId and is not confirmed delivered.

    Its owner may be mid-delivery, or may have given up. The gate answers 503 and
    the payer retries; it is never the final 402.
    """


class ReconcileBusyError(Exception):
    """A request holds the payment: its own steps write the record."""


class ClaimState(Enum):
    NONE = "none"  # no claim, or one this process gave up: nothing was delivered
    DELIVERED = "delivered"  # a claim marked delivered: the final 402
    HELD = "held"  # a claim of another request without the delivered mark


@dataclass
class _Pending:
    expires: float
    next_log: float


class PendingTable:
    """Every token remembered for a key.

    A second give-up for the same key must not make the first one forgotten while
    it is still in the store.
    """

    def __init__(self) -> None:
        self.by_key: dict[str, dict[str, _Pending]] = {}
        self.size = 0

    def get(self, key: str, token: str) -> _Pending | None:
        return self.by_key.get(key, {}).get(token)

    def put(self, key: str, token: str, entry: _Pending) -> None:
        tokens = self.by_key.setdefault(key, {})
        if token not in tokens:
            self.size += 1
        tokens[token] = entry

    def delete(self, key: str, token: str) -> None:
        tokens = self.by_key.get(key)
        if tokens is None or token not in tokens:
            return
        del tokens[token]
        self.size -= 1
        if not tokens:
            del self.by_key[key]

    def purge(self, now: float) -> None:
        for key, tokens in list(self.by_key.items()):
            for token, entry in list(tokens.items()):
                if now >= entry.expires:
                    self.delete(key, token)


class Ledger:
    def __init__(
        self,
        store: Store,
        logger: logging.Logger,
        clock: Callable[[], float] = time.monotonic,
        wall_clock: Callable[[], float] = time.time,
    ) -> None:
        self.store = store
        self._log = logger
        self._clock = clock
        self.wall_clock = wall_clock
        self._mutex = threading.Lock()
        self._fallback: dict[str, tuple[Record, float]] = {}
        # Claims this process took and gave up: a claim whose reply was lost, or
        # whose release failed. They are not a delivery. The reconciler keeps trying
        # to delete them, and a retry on this process takes them back at once.
        self._claims = PendingTable()
        # Locks whose release failed (tokens by hash).
        self._locks = PendingTable()
        self._last_record_log = float("-inf")
        forksafe.register(self)

    def reset_after_fork(self) -> None:
        """In a forked child: a fresh mutex, and none of the parent's pending work."""
        self._mutex = threading.Lock()
        self._claims = PendingTable()
        self._locks = PendingTable()
        self._fallback = {}

    # -- locks ------------------------------------------------------------

    def lock(self, hash: str, token: str, ttl: float) -> bool:
        """Take the lock.

        When the store answers with an error the lock may still have been applied
        (a lost reply), so the holder is read back: our token means we hold it, an
        absent key means try again, and when the holder cannot be read the lock is
        released (in the background if it must be) so it does not block the payer's
        retry. A lock of this process that could not be released earlier is taken back.
        """
        last_error: Exception | None = None
        for attempt in range(AMBIGUOUS_RETRIES):
            try:
                acquired = self.store.lock(hash, token, ttl)
            except StoreFullError:
                raise
            except Exception as error:
                last_error = error
                holder = self._read_lock_holder(hash, token, ttl, error)
                if holder is not None:
                    return holder == token
                continue
            if acquired:
                return True
            if attempt == 0 and not self._has_pending_lock(hash):
                # A client library that re-sends a SET NX after a lost reply (redis-py 6+ retries by
                # default) gets "taken" for a lock it holds itself: read the holder back, once.
                # A failing read-back releases (or remembers the release of) the lock it may hold.
                return self._read_lock_holder(hash, token, ttl, None) == token
            # "Taken" after an ambiguous first try, or by a lock of ours that was left behind.
            holder = self._read_lock_holder(hash, token, ttl, None)
            if holder is not None and holder != token and self._is_pending_lock(hash, holder):
                self.store.unlock(hash, holder)  # still held: the reconciler keeps trying
                self._forget_lock(hash, holder)
                continue
            return holder == token
        self.unlock(hash, token, ttl)
        assert last_error is not None
        raise last_error

    def _read_lock_holder(
        self, hash: str, token: str, ttl: float, cause: Exception | None
    ) -> str | None:
        try:
            return self.store.lock_holder(hash)
        except Exception as read_error:
            self.unlock(hash, token, ttl)  # it may have applied
            if cause is None:
                raise
            raise read_error from cause

    def unlock(self, hash: str, token: str, ttl: float) -> None:
        """Release the lock. If the store cannot, the release is retried in the
        background until it succeeds or the lock's own TTL ends, so a retry of the
        payment is not held at "payment in progress" for the whole TTL."""
        try:
            self.store.unlock(hash, token)
        except Exception as error:
            self._remember(self._locks, hash, token, ttl)
            event(
                self._log,
                logging.ERROR,
                "store unlock failed: retried in the background",
                canonicalHash=hash,
                error=str(error),
            )

    # -- claims -----------------------------------------------------------

    def claim(self, update_id: str, token: str) -> bool:
        """Take the delivery claim.

        True: it is ours. False: the updateId is confirmed delivered (the claim
        carries the delivered mark): the final answer. A claim whose reply was lost
        is resolved by reading the holder; when it cannot be resolved the claim is
        released, and if even that fails the token is remembered and the claim is
        taken back when the payment is retried. A claim of another request that is
        not marked delivered raises :class:`ClaimHeldError`, and a holder that cannot
        be read raises: neither is the final answer.
        """
        last_error: Exception | None = None
        ambiguous = False
        for _ in range(AMBIGUOUS_RETRIES + 1):
            claim_error: Exception | None = None
            claimed = False
            try:
                claimed = self.store.claim(update_id, token, RECORD_TTL_SECONDS)
            except Exception as error:
                claim_error = last_error = error
                ambiguous = True
            if claimed:
                return True
            try:
                holder = self.store.claimant(update_id)
            except Exception as read_error:
                if ambiguous:
                    self.unclaim(update_id, token)
                    raise read_error from claim_error
                raise  # taken by a holder that cannot be read: not the final answer
            if holder == token:
                return True
            if holder is None:
                continue  # a lost reply that did not apply: try again
            if is_delivered_mark(holder):
                return False  # confirmed delivered: the final answer
            if self._is_withheld(update_id, holder):
                # A claim this process gave up earlier is not a delivery: take it back.
                self.store.unclaim(update_id, holder)
                self._forget_claim(update_id, holder)
                continue
            # A claim of another request, not confirmed delivered. Read once more: it may
            # be a claim of ours the reconciler has just released and forgotten.
            again = self.store.claimant(update_id)
            if again == holder:
                raise ClaimHeldError(update_id)
        self.unclaim(update_id, token)
        raise last_error or RuntimeError("claim could not be resolved")

    def unclaim(self, update_id: str, token: str) -> None:
        """Give a claim back. If the store cannot, the token is remembered: the
        reconciler keeps deleting it, and a retry on this process takes it back at once."""
        try:
            self.store.unclaim(update_id, token)
        except Exception as error:
            stored = self._remember(self._claims, update_id, token, RECORD_TTL_SECONDS)
            event(
                self._log,
                logging.ERROR,
                "claim could not be released: retried in the background, taken back on retry",
                updateId=update_id,
                remembered=stored,
                error=str(error),
            )

    def mark_delivered(self, update_id: str, token: str) -> None:
        """Record that the response reached the client. If it fails the claim stays
        unmarked, which only makes replays answer 503 instead of 402."""
        try:
            self.store.mark_delivered(update_id, token, RECORD_TTL_SECONDS)
        except Exception as error:
            event(
                self._log,
                logging.ERROR,
                "delivery not marked: replays answer 503 until the claim lapses",
                updateId=update_id,
                error=str(error),
            )

    def claim_state(self, update_id: str) -> ClaimState:
        """Read the claim on an updateId.

        A holder this process does not know is confirmed by a second read: the
        reconciler deletes a given-up claim in the store before it forgets the token,
        so a holder read just before that delete must not be taken for a delivery.
        """
        for _ in range(AMBIGUOUS_RETRIES):
            holder = self.store.claimant(update_id)
            if holder is None or self._is_withheld(update_id, holder):
                return ClaimState.NONE
            if is_delivered_mark(holder):
                return ClaimState.DELIVERED
            again = self.store.claimant(update_id)
            if again is None:
                return ClaimState.NONE
            if again != holder:
                continue  # changed between the reads: classify the new holder
            if self._is_withheld(update_id, again):
                return ClaimState.NONE
            return ClaimState.HELD
        return ClaimState.HELD

    # -- records ----------------------------------------------------------

    def lookup(self, hash: str) -> Record | None:
        """The record of a payment. The fallback holds writes the store refused; a
        settled record beats an attempt, and the store wins a tie."""
        fallback = self._from_fallback(hash)
        try:
            record = self.store.record(hash)
        except Exception:
            if fallback is None:
                raise
            return fallback
        if fallback is None:
            return record
        if record is None or (fallback.state == STATE_SETTLED and record.state != STATE_SETTLED):
            return fallback
        if record.state == STATE_SETTLED:
            self._drop_fallback(hash)
        return record

    def save_attempt(self, hash: str, record: Record) -> None:
        """Record that /settle is about to run. An existing record is kept: an
        attempt never downgrades a settled payment or replaces another payload's.
        When the store cannot even be read, the attempt is kept in process and
        still written to the store if it will take it."""
        try:
            existing = self.lookup(hash)
        except Exception:
            # Only the in-process fallback: a blind write could overwrite an older attempt (and its
            # age) or a settled record the store holds. The reconciler writes it under the payment's lock.
            self._to_fallback(hash, record)
            raise
        if existing is None:
            self._write(hash, record)

    def save_settled(self, hash: str, record: Record) -> None:
        """Record the settled payment.

        It refuses to overwrite a settled record of a different updateId. A settled
        record of the same updateId is kept as it is (its digest may come from another
        /supported pin: the facilitator confirmed the transaction either way). An
        attempt, of any bytes, is replaced by the settled record. When the store cannot
        be read the record is kept in process and
        the error is raised: the caller withholds the delivery, and the retry is served.
        """
        try:
            existing = self.lookup(hash)
        except Exception:
            self._to_fallback(hash, record)
            raise
        if existing is not None and existing.state == STATE_SETTLED:
            if existing.update_id != record.update_id:
                raise RecordConflictError("a different payment is already recorded under this hash")
            return
        self._write(hash, record)

    def _write(self, hash: str, record: Record) -> None:
        try:
            self.store.put_record(hash, record, RECORD_TTL_SECONDS)
        except Exception:
            self._to_fallback(hash, record)
            raise
        self._drop_fallback(hash)

    def clear(self, hash: str) -> None:
        """Remove an attempt record after a definite refusal, in one delete under the
        request's own lock. If the store cannot delete it the attempt stays until its TTL: a
        stale attempt only makes a retry of those bytes skip /verify and ask /settle, which
        answers from the facilitator's own record. (A remembered clear that hid the attempt
        from lookups and deleted it later could remove a newer attempt of the same bytes.)"""
        self._drop_fallback(hash)
        try:
            self.store.delete_record(hash)
        except Exception as error:
            event(
                self._log,
                logging.ERROR,
                "attempt record could not be cleared: it stays until its TTL",
                canonicalHash=hash,
                error=str(error),
            )

    # -- in-process tables ------------------------------------------------

    def _from_fallback(self, hash: str) -> Record | None:
        with self._mutex:
            entry = self._fallback.get(hash)
            if entry is None or self._clock() >= entry[1]:
                self._fallback.pop(hash, None)
                return None
            return entry[0]

    def _to_fallback(self, hash: str, record: Record) -> None:
        with self._mutex:
            if hash not in self._fallback and len(self._fallback) >= MAX_FALLBACK_RECORDS:
                now = self._clock()
                for key in [k for k, (_, expires) in self._fallback.items() if now >= expires]:
                    del self._fallback[key]
                if len(self._fallback) >= MAX_FALLBACK_RECORDS:
                    event(
                        self._log,
                        logging.ERROR,
                        "fallback record table is full: payment not remembered in process",
                        canonicalHash=hash,
                    )
                    return
            self._fallback[hash] = (record, self._clock() + RECORD_TTL_SECONDS)

    def _drop_fallback(self, hash: str) -> None:
        with self._mutex:
            self._fallback.pop(hash, None)

    def _drop_fallback_if(self, hash: str, record: Record) -> None:
        with self._mutex:
            entry = self._fallback.get(hash)
            if entry is not None and entry[0] == record:
                del self._fallback[hash]

    def _remember(self, table: PendingTable, key: str, token: str, ttl: float) -> bool:
        """Store a pending release. Returns whether it was stored."""
        with self._mutex:
            now = self._clock()
            if table.get(key, token) is None and table.size >= MAX_PENDING_RELEASES:
                table.purge(now)
                if table.size >= MAX_PENDING_RELEASES:
                    return False
            table.put(key, token, _Pending(now + ttl, now + RECONCILE_LOG_EVERY_SECONDS))
            return True

    def _is_withheld(self, update_id: str, token: str) -> bool:
        with self._mutex:
            entry = self._claims.get(update_id, token)
            return entry is not None and self._clock() < entry.expires

    def _is_pending_lock(self, hash: str, token: str) -> bool:
        with self._mutex:
            entry = self._locks.get(hash, token)
            return entry is not None and self._clock() < entry.expires

    def _has_pending_lock(self, hash: str) -> bool:
        with self._mutex:
            return bool(self._locks.by_key.get(hash))

    def _forget_claim(self, update_id: str, token: str) -> None:
        with self._mutex:
            self._claims.delete(update_id, token)

    def _forget_lock(self, hash: str, token: str) -> None:
        with self._mutex:
            self._locks.delete(hash, token)

    # -- the reconciler's work --------------------------------------------

    def left_behind(self) -> dict[str, int]:
        """What this process still holds for the store: claims, locks, fallback records, clears."""
        with self._mutex:
            return {
                "claims": self._claims.size,
                "locks": self._locks.size,
                "records": len(self._fallback),
            }

    def has_pending(self) -> bool:
        return any(self.left_behind().values())

    def reconcile_once(self, final: bool = False) -> None:
        """One pass over what a store failure left behind. Each item is retried until it
        succeeds or its TTL ends; what is still pending is logged at error.

        ``final`` (the pass at shutdown, which a slow store may cut short): the records go first,
        because a settled record that never reaches the store makes a later retry be refused as
        expired, while a claim or lock that is not released ends with its TTL."""
        if final:
            self._reconcile_records()
        for key, token, due in self._snapshot(self._claims):
            try:
                self.store.unclaim(key, token)
            except Exception as error:
                if due:
                    event(
                        self._log,
                        logging.ERROR,
                        "claim still not released: payment not served until the store answers",
                        updateId=key,
                        error=str(error),
                    )
            else:
                self._forget_claim(key, token)
        for key, token, due in self._snapshot(self._locks):
            try:
                self.store.unlock(key, token)
            except Exception as error:
                if due:
                    event(
                        self._log,
                        logging.ERROR,
                        "lock still not released",
                        canonicalHash=key,
                        error=str(error),
                    )
            else:
                self._forget_lock(key, token)
        if not final:
            self._reconcile_records()

    def _snapshot(self, table: PendingTable) -> list[tuple[str, str, bool]]:
        items: list[tuple[str, str, bool]] = []
        with self._mutex:
            now = self._clock()
            for key, tokens in list(table.by_key.items()):
                for token, entry in list(tokens.items()):
                    if now >= entry.expires:
                        table.delete(key, token)
                        continue
                    due = now >= entry.next_log
                    if due:
                        entry.next_log = now + RECONCILE_LOG_EVERY_SECONDS
                    items.append((key, token, due))
        return items

    def _reconcile_records(self) -> None:
        """Write records that exist only in the fallback to the store, under the payment's own lock."""
        with self._mutex:
            now = self._clock()
            pending = {h: rec for h, (rec, expires) in self._fallback.items() if now < expires}
            for hash in [h for h, (_, expires) in self._fallback.items() if now >= expires]:
                del self._fallback[hash]
        for hash, record in pending.items():
            try:
                self._with_payment_lock(hash, partial(self._write_fallback_record, hash, record))
            except ReconcileBusyError:
                continue
            except Exception as error:
                if self._record_log_due():
                    event(
                        self._log,
                        logging.ERROR,
                        "payment record still only in process memory",
                        canonicalHash=hash,
                        state=record.state,
                        updateId=record.update_id,
                        error=str(error),
                    )

    def _write_fallback_record(self, hash: str, record: Record) -> None:
        # Re-check under the lock: the request path may have changed it since the snapshot.
        with self._mutex:
            entry = self._fallback.get(hash)
            if entry is None or entry[0] != record:
                return
        stored = self.store.record(hash)
        if stored is None or (stored.state != STATE_SETTLED and record.state == STATE_SETTLED):
            self.store.put_record(hash, record, RECORD_TTL_SECONDS)
        self._drop_fallback_if(hash, record)

    def _holder_is(self, hash: str, token: str) -> bool:
        try:
            return self.store.lock_holder(hash) == token
        except Exception:
            return False

    def _with_payment_lock(self, hash: str, work: Callable[[], None]) -> None:
        """Run ``work`` holding the payment's own store lock; a payment a request holds is skipped."""
        token = new_token()
        try:
            acquired = self.store.lock(hash, token, RECONCILE_LOCK_TTL_SECONDS)
        except Exception:
            self.unlock(hash, token, RECONCILE_LOCK_TTL_SECONDS)  # it may have applied
            raise
        if not acquired and self._holder_is(hash, token):
            acquired = True  # a client that re-sent the SET NX got "taken" for its own lock
        if not acquired:
            raise ReconcileBusyError(hash)
        try:
            work()
        finally:
            self.unlock(hash, token, RECONCILE_LOCK_TTL_SECONDS)

    def _record_log_due(self) -> bool:
        with self._mutex:
            now = self._clock()
            if now >= self._last_record_log + RECONCILE_LOG_EVERY_SECONDS:
                self._last_record_log = now
                return True
            return False
