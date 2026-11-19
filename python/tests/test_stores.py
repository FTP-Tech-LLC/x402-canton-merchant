"""Store contract (memory and Redis over a fake client) and the ledger's handling of lost replies."""

from __future__ import annotations

import logging
import threading
from concurrent.futures import ThreadPoolExecutor

import pytest

from ftptech_x402_canton import MemoryStore, RedisStore, Store, StoreFullError
from ftptech_x402_canton.ledger import ClaimHeldError, ClaimState, Ledger, RecordConflictError
from ftptech_x402_canton.store import RECORD_TTL_SECONDS, STATE_ATTEMPT, STATE_SETTLED, Record
from ftptech_x402_canton.store_memory import LOCK_HEADROOM
from ftptech_x402_canton.store_redis import DEFAULT_REDIS_PREFIX
from tests.stores import FakeRedis, SpyStore


class Clock:
    def __init__(self) -> None:
        self.now = 1000.0

    def __call__(self) -> float:
        return self.now


def memory(clock: Clock | None = None, **kwargs: int) -> Store:
    return MemoryStore(clock=clock or Clock(), **kwargs)


def redis() -> Store:
    return RedisStore(FakeRedis(), "t:")


@pytest.fixture(params=["memory", "redis"])
def store(request: pytest.FixtureRequest) -> Store:
    return memory() if request.param == "memory" else redis()


REC = Record(STATE_SETTLED, "digest", "1220u1", "payer::1")


def test_lock_is_atomic_and_owned(store: Store) -> None:
    assert store.lock("h", "a", 60) is True
    assert store.lock("h", "b", 60) is False
    assert store.lock_holder("h") == "a"
    store.unlock("h", "b")  # not the owner: nothing happens
    assert store.lock_holder("h") == "a"
    store.unlock("h", "a")
    assert store.lock_holder("h") is None
    assert store.lock("h", "b", 60) is True


def test_claim_is_atomic_and_owned(store: Store) -> None:
    assert store.claim("u", "a", 60) is True
    assert store.claim("u", "b", 60) is False
    assert store.claimant("u") == "a"
    store.unclaim("u", "b")
    assert store.claimant("u") == "a"
    store.unclaim("u", "a")
    assert store.claimant("u") is None and store.claim("u", "b", 60) is True


def test_record_roundtrip_overwrite_and_delete(store: Store) -> None:
    assert store.record("h") is None
    store.put_record("h", Record(STATE_ATTEMPT, "d1"), 60)
    assert store.record("h") == Record(STATE_ATTEMPT, "d1")
    store.put_record("h", REC, 60)
    assert store.record("h") == REC
    store.delete_record("h")
    assert store.record("h") is None


def test_locks_records_and_claims_are_separate_namespaces(store: Store) -> None:
    store.lock("x", "t", 60)
    store.put_record("x", REC, 60)
    store.claim("x", "c", 60)
    assert store.lock_holder("x") == "t" and store.claimant("x") == "c" and store.record("x") == REC


def test_concurrent_claims_have_one_winner(store: Store) -> None:
    barrier = threading.Barrier(16)

    def go(n: int) -> bool:
        barrier.wait()
        return store.claim("u", f"t{n}", 60)

    with ThreadPoolExecutor(16) as pool:
        assert sum(pool.map(go, range(16))) == 1


def test_concurrent_locks_have_one_winner(store: Store) -> None:
    barrier = threading.Barrier(16)

    def go(n: int) -> bool:
        barrier.wait()
        return store.lock("h", f"t{n}", 60)

    with ThreadPoolExecutor(16) as pool:
        assert sum(pool.map(go, range(16))) == 1


def test_memory_entries_expire_by_ttl() -> None:
    clock = Clock()
    store = memory(clock)
    store.lock("h", "a", 10)
    store.put_record("h", REC, 20)
    store.claim("u", "a", 30)
    clock.now += 11
    assert (
        store.lock_holder("h") is None and store.record("h") == REC and store.claimant("u") == "a"
    )
    assert store.lock("h", "b", 10) is True  # an expired lock is takeable
    clock.now += 15  # t = 26 s: the record (20 s) is gone, the claim (30 s) is not
    assert store.record("h") is None and store.claimant("u") == "a"
    clock.now += 10
    assert store.claimant("u") is None


def test_memory_store_default_bound_matches_the_documentation() -> None:
    from ftptech_x402_canton.store_memory import DEFAULT_MAX_ENTRIES

    assert DEFAULT_MAX_ENTRIES == 200_000


def test_memory_store_admits_a_payment_only_with_room_for_its_record_and_claim() -> None:
    store = memory(max_entries=6)
    # Two admitted payments take lock + record + claim each: exactly 6 entries.
    for n in range(2):
        assert store.lock(f"h{n}", "tok", 60) is True
        store.put_record(f"h{n}", Record(STATE_SETTLED, "d", f"u{n}"), RECORD_TTL_SECONDS)
        assert store.claim(f"u{n}", "tok", RECORD_TTL_SECONDS) is True
    with pytest.raises(StoreFullError):
        store.lock("h-new", "tok", 60)  # a full store refuses a new payment at admission
    assert (
        store.claimant("u0") == "tok" and store.claimant("u1") == "tok"
    )  # no live entry was evicted


def test_memory_store_never_refuses_the_record_or_claim_of_an_admitted_payment() -> None:
    store = memory(max_entries=3)
    assert store.lock("h", "tok", 60) is True  # room for lock, record and claim
    for n in range(10):  # other payments crowd the store while this one works
        store.claim(f"other{n}", "t", RECORD_TTL_SECONDS)
    store.put_record("h", Record(STATE_SETTLED, "d", "u"), RECORD_TTL_SECONDS)  # never refused
    assert store.claim("u", "tok", RECORD_TTL_SECONDS) is True  # never refused
    assert store.lock_holder("h") == "tok"


def test_memory_store_purge_on_a_full_store_is_rate_limited() -> None:
    clock = Clock()
    store = MemoryStore(max_entries=30, clock=clock)
    for n in range(28):
        store.claim(f"u{n}", "t", RECORD_TTL_SECONDS)
    with pytest.raises(StoreFullError):
        store.lock("h1", "t", 60)
    first = store._last_purge  # noqa: SLF001
    with pytest.raises(StoreFullError):
        store.lock("h2", "t", 60)
    assert store._last_purge == first  # noqa: SLF001  # a second refusal inside the interval does not scan again
    clock.now += 2
    with pytest.raises(StoreFullError):
        store.lock("h3", "t", 60)
    assert store._last_purge > first  # noqa: SLF001


def test_memory_store_replacing_a_record_needs_no_room() -> None:
    store = memory(max_entries=3)
    store.put_record("h", Record(STATE_ATTEMPT, "d"), 60)
    store.claim("a", "t", 60)
    store.claim("b", "t", 60)
    store.put_record("h", Record(STATE_SETTLED, "d", "u"), 60)  # attempt to settled on a full store
    assert store.record("h") == Record(STATE_SETTLED, "d", "u")


def test_memory_store_makes_room_by_dropping_expired_entries_never_live_ones() -> None:
    clock = Clock()
    store = memory(clock, max_entries=6)
    store.claim("keep", "tok", RECORD_TTL_SECONDS)
    for n in range(3):
        store.claim(f"short{n}", "tok", 1)
    clock.now += 2
    assert store.lock("new", "tok", 60) is True  # expired entries made room ...
    assert store.claimant("keep") == "tok"  # ... and the live claim stayed


def test_memory_store_bound_must_fit_one_payment() -> None:
    with pytest.raises(ValueError):
        MemoryStore(max_entries=LOCK_HEADROOM)
    MemoryStore(max_entries=LOCK_HEADROOM + 1)


def test_redis_store_uses_the_documented_keys_and_ttls() -> None:
    client = FakeRedis()
    store = RedisStore(client, "x402:app:")
    store.lock("hh", "tok", 200.2)
    store.put_record("hh", REC, RECORD_TTL_SECONDS)
    store.claim("uu", "tok", RECORD_TTL_SECONDS)
    assert client.ttls == {
        "x402:app:lock:hh": 201,
        "x402:app:record:hh": 93600,
        "x402:app:claimed:uu": 93600,
    }
    assert client.data["x402:app:lock:hh"] == "tok" and client.data["x402:app:claimed:uu"] == "tok"
    assert [op for op, key in client.ops if key.endswith("lock:hh")] == [
        "set_nx"
    ]  # SET NX EX for locks
    assert [op for op, key in client.ops if key.endswith("record:hh")] == [
        "set"
    ]  # plain SET EX for records
    store.unlock("hh", "tok")
    store.unclaim("uu", "tok")
    assert [op for op, _ in client.ops][-2:] == [
        "delete_if_value",
        "delete_if_value",
    ]  # compare-and-delete


def test_redis_store_never_uses_a_bare_key() -> None:
    client = FakeRedis()
    store = RedisStore(client)  # no prefix given
    store.lock("h", "t", 5)
    store.put_record("h", REC, 5)
    store.claim("u", "t", 5)
    store.record("h")
    store.lock_holder("h")
    store.claimant("u")
    store.delete_record("h")
    store.unlock("h", "t")
    store.unclaim("u", "t")
    assert client.ops and all(key.startswith(DEFAULT_REDIS_PREFIX) for _, key in client.ops)


def test_redis_store_propagates_errors() -> None:
    client = FakeRedis()
    store = RedisStore(client, "p:")
    client.error = ConnectionError("down")
    for call in (
        lambda: store.lock("h", "t", 5), lambda: store.lock_holder("h"), lambda: store.unlock("h", "t"),
        lambda: store.record("h"), lambda: store.put_record("h", REC, 5), lambda: store.delete_record("h"),
        lambda: store.claim("u", "t", 5), lambda: store.claimant("u"), lambda: store.unclaim("u", "t"),
    ):  # fmt: skip
        with pytest.raises(ConnectionError):
            call()


def test_a_corrupt_stored_record_is_an_error_not_a_free_payment() -> None:
    client = FakeRedis()
    client.data["p:record:h"] = "not json"
    with pytest.raises(ValueError):
        RedisStore(client, "p:").record("h")
    client.data["p:record:h"] = '{"no":"state"}'
    with pytest.raises(ValueError):
        RedisStore(client, "p:").record("h")


# -- the ledger: lost replies ----------------------------------------------------------


def ledger_with(spy: SpyStore) -> Ledger:
    return Ledger(spy, logging.getLogger("test-ledger"))


def test_lock_whose_reply_was_lost_is_held_and_released() -> None:
    spy = SpyStore()
    spy.fail("lock", "apply_raise")
    ledger = ledger_with(spy)
    assert ledger.lock("h", "mine", 60) is True  # the read-back shows our token
    ledger.unlock("h", "mine", 60)
    assert spy.inner.lock_holder("h") is None


def test_a_lost_reply_on_another_holders_key_is_not_mistaken_for_ours() -> None:
    spy = SpyStore()
    spy.inner.lock("h", "theirs", 60)
    spy.fail("lock", "raise")
    assert ledger_with(spy).lock("h", "mine", 60) is False  # NOT acquired: someone else holds it
    spy2 = SpyStore()
    spy2.inner.claim("u", "theirs", 60)
    spy2.fail("claim", "raise")
    with pytest.raises(
        ClaimHeldError
    ):  # another request's claim, not marked delivered: never "ours", never final
        ledger_with(spy2).claim("u", "mine")


def test_lock_error_without_effect_is_retried() -> None:
    spy = SpyStore()
    spy.fail("lock", "raise")
    assert ledger_with(spy).lock("h", "mine", 60) is True
    assert spy.names().count("lock") == 2


def test_ambiguous_lock_error_with_an_unreadable_key_stays_an_error_and_is_released() -> None:
    spy = SpyStore()
    spy.fail("lock", "apply_raise")
    spy.fail("lock_holder", "raise")
    with pytest.raises(ConnectionError):
        ledger_with(spy).lock("h", "mine", 60)
    assert spy.inner.lock_holder("h") is None  # given up best effort: it does not block the retry


def test_lock_that_is_taken_after_an_ambiguous_first_try_is_read_back() -> None:
    spy = SpyStore()
    spy.fail("lock", "raise")  # the first reply is lost and does not apply
    spy.fail("lock_holder", "ok")
    ledger = ledger_with(spy)
    spy.inner.lock("h", "theirs", 60)
    assert ledger.lock("h", "mine", 60) is False


def test_claim_whose_reply_was_lost_is_still_delivered() -> None:
    spy = SpyStore()
    spy.fail("claim", "apply_raise")
    assert ledger_with(spy).claim("u", "mine") is True
    assert spy.inner.claimant("u") == "mine"


def test_claim_whose_reply_was_lost_and_whose_readback_fails_is_released_not_left_behind() -> None:
    spy = SpyStore()
    spy.fail("claim", "apply_raise", "apply_raise", "apply_raise")
    spy.fail("claimant", "raise", "raise", "raise")
    with pytest.raises(ConnectionError):
        ledger_with(spy).claim("u", "mine")
    assert spy.inner.claimant("u") is None  # not left behind a withheld delivery


def test_a_claim_that_cannot_be_released_is_remembered_and_taken_back_on_retry() -> None:
    spy = SpyStore()
    spy.fail("claim", "apply_raise")
    spy.fail("claimant", "raise")
    spy.fail("unclaim", "raise")
    ledger = ledger_with(spy)
    with pytest.raises(ConnectionError):
        ledger.claim("u", "mine")
    assert spy.inner.claimant("u") == "mine"  # stuck in the store ...
    assert ledger.claim_state("u") is ClaimState.NONE  # ... but not a delivery
    assert ledger.claim("u", "next") is True  # the retry takes it back
    assert spy.inner.claimant("u") == "next"


def test_a_taken_claim_whose_holder_cannot_be_read_is_never_the_final_answer() -> None:
    spy = SpyStore()
    spy.inner.claim("u", "theirs", 60)
    spy.fail("claimant", "raise")
    with pytest.raises(ConnectionError):  # the gate withholds with 503; it never says "delivered"
        ledger_with(spy).claim("u", "mine")


def test_only_a_claim_marked_delivered_is_the_final_answer() -> None:
    spy = SpyStore()
    ledger = ledger_with(spy)
    spy.inner.claim("u", "theirs", 60)
    assert ledger.claim_state("u") is ClaimState.HELD
    with pytest.raises(ClaimHeldError):
        ledger.claim("u", "mine")
    spy.inner.mark_delivered("u", "theirs", 60)
    assert ledger.claim_state("u") is ClaimState.DELIVERED
    assert ledger.claim("u", "mine") is False
    assert ledger.claim_state("absent") is ClaimState.NONE


def test_mark_delivered_replaces_the_claim_value_and_keeps_the_ttl_rule(store: Store) -> None:
    assert store.claim("u", "tok", 60) is True
    store.mark_delivered("u", "tok", 60)
    assert store.claimant("u") == "delivered:tok"
    assert store.claim("u", "other", 60) is False


def test_a_failed_mark_leaves_the_claim_unmarked_and_logged(
    caplog: pytest.LogCaptureFixture,
) -> None:
    spy = SpyStore()
    spy.inner.claim("u", "tok", 60)
    spy.fail("mark_delivered", "raise")
    ledger_with(spy).mark_delivered("u", "tok")
    assert spy.inner.claimant("u") == "tok"
    assert any("delivery not marked" in r.getMessage() for r in caplog.records)


def test_unlock_and_unclaim_do_not_touch_another_holder() -> None:
    spy = SpyStore()
    spy.inner.lock("h", "theirs", 60)
    spy.inner.claim("u", "theirs", 60)
    ledger = ledger_with(spy)
    ledger.unlock("h", "mine", 60)
    ledger.unclaim("u", "mine")
    assert spy.inner.lock_holder("h") == "theirs" and spy.inner.claimant("u") == "theirs"


# -- the ledger: records -----------------------------------------------------------------


def test_a_settled_record_is_never_overwritten_by_another_payment() -> None:
    spy = SpyStore()
    ledger = ledger_with(spy)
    ledger.save_settled("h", Record(STATE_SETTLED, "d1", "u1", "p"))
    with pytest.raises(RecordConflictError):
        ledger.save_settled("h", Record(STATE_SETTLED, "d2", "u2", "p"))  # another payload
    with pytest.raises(RecordConflictError):
        ledger.save_settled(
            "h", Record(STATE_SETTLED, "d1", "u2", "p")
        )  # same bytes, another updateId
    assert spy.inner.record("h") == Record(STATE_SETTLED, "d1", "u1", "p")
    ledger.save_settled(
        "h", Record(STATE_SETTLED, "d1", "u1", "p")
    )  # the same payment again is fine


def test_an_attempt_never_downgrades_a_settled_record_or_replaces_another_payloads() -> None:
    spy = SpyStore()
    ledger = ledger_with(spy)
    ledger.save_settled("h", Record(STATE_SETTLED, "d1", "u1"))
    ledger.save_attempt("h", Record(STATE_ATTEMPT, "d1"))
    assert spy.inner.record("h") == Record(STATE_SETTLED, "d1", "u1")
    ledger.save_attempt("g", Record(STATE_ATTEMPT, "dA"))
    ledger.save_attempt("g", Record(STATE_ATTEMPT, "dB"))
    assert spy.inner.record("g") == Record(STATE_ATTEMPT, "dA")


def test_the_fallback_serves_reads_when_the_store_is_down_and_writes_when_it_refuses() -> None:
    spy = SpyStore()
    ledger = ledger_with(spy)
    spy.down = {"put_record"}
    with pytest.raises(ConnectionError):
        ledger.save_settled("h", Record(STATE_SETTLED, "d", "u"))
    spy.down_all()  # now even reads fail
    assert ledger.lookup("h") == Record(STATE_SETTLED, "d", "u")  # still seen: in-process fallback
    with pytest.raises(ConnectionError):
        ledger.lookup("other")  # nothing known: the error is honest
    spy.up()
    assert ledger.lookup("h") == Record(STATE_SETTLED, "d", "u")
    ledger.reconcile_once()  # the store takes it: the fallback is dropped
    assert spy.inner.record("h") is not None and ledger.left_behind()["records"] == 0


def test_a_settled_fallback_beats_an_attempt_in_the_store() -> None:
    spy = SpyStore()
    ledger = ledger_with(spy)
    spy.inner.put_record("h", Record(STATE_ATTEMPT, "d"), 60)
    spy.fail("put_record", "raise")
    with pytest.raises(ConnectionError):
        ledger.save_settled("h", Record(STATE_SETTLED, "d", "u"))
    assert ledger.lookup("h") == Record(STATE_SETTLED, "d", "u")


def test_clear_removes_the_record_and_a_failed_clear_leaves_it_until_its_ttl() -> None:
    spy = SpyStore()
    ledger = ledger_with(spy)
    ledger.save_attempt("h", Record(STATE_ATTEMPT, "d1"))
    ledger.clear("h")
    assert spy.inner.record("h") is None
    ledger.save_attempt("g", Record(STATE_ATTEMPT, "d2"))
    spy.fail("delete_record", "raise")
    ledger.clear("g")
    assert spy.inner.record("g") == Record(STATE_ATTEMPT, "d2")  # stays; no background delete
    assert ledger.lookup("g") == Record(STATE_ATTEMPT, "d2")  # and is not hidden from lookups
    assert not ledger.has_pending()


def test_a_failed_clear_never_deletes_a_newer_attempt_of_the_same_bytes() -> None:
    spy = SpyStore()
    ledger = ledger_with(spy)
    ledger.save_attempt("g", Record(STATE_ATTEMPT, "same-bytes", at=1))
    spy.fail("delete_record", "raise")
    ledger.clear("g")  # the first attempt's clear fails
    # The same bytes come back and a new attempt is recorded (kept: one is there already) ...
    ledger.save_attempt("g", Record(STATE_ATTEMPT, "same-bytes", at=2))
    ledger.reconcile_once()  # ... and nothing in the background removes or hides it
    assert spy.inner.record("g") is not None and ledger.lookup("g") is not None


def test_a_failed_read_never_overwrites_the_stored_attempt_or_its_age() -> None:
    spy = SpyStore()
    ledger = ledger_with(spy)
    old = Record(STATE_ATTEMPT, "d", at=1000)
    spy.inner.put_record("h", old, 3600)
    spy.fail("record", "raise")
    with pytest.raises(ConnectionError):
        ledger.save_attempt("h", Record(STATE_ATTEMPT, "d", at=999999))
    assert spy.inner.record("h") == old  # no blind write: the old age stays


# -- the reconciler ----------------------------------------------------------------------


def test_every_token_given_up_for_one_key_is_remembered_and_released() -> None:
    spy = SpyStore()
    ledger = ledger_with(spy)
    spy.inner.claim("u", "first", 60)
    spy.fail("unclaim", "raise", "raise")
    ledger.unclaim("u", "first")
    spy.inner.unclaim("u", "first")  # (simulate the claim lapsing)
    spy.inner.claim("u", "second", 60)
    ledger.unclaim("u", "second")  # a second give-up for the same key must not replace the first
    assert ledger.left_behind()["claims"] == 2
    spy.inner.unclaim("u", "second")
    spy.inner.claim("u", "first", 60)  # the first one is back in the store (a late applied write)
    ledger.reconcile_once()
    assert spy.inner.claimant("u") is None and ledger.left_behind()["claims"] == 0


def test_the_same_holds_for_locks() -> None:
    spy = SpyStore()
    ledger = ledger_with(spy)
    spy.inner.lock("h", "a", 60)
    spy.fail("unlock", "raise")
    ledger.unlock("h", "a", 60)
    spy.inner.unlock("h", "a")
    spy.inner.lock("h", "b", 60)
    spy.fail("unlock", "raise")
    ledger.unlock("h", "b", 60)
    assert ledger.left_behind()["locks"] == 2
    ledger.reconcile_once()
    assert spy.inner.lock_holder("h") is None and ledger.left_behind()["locks"] == 0


def test_a_lock_left_behind_by_a_failed_release_is_taken_back_by_the_retry() -> None:
    spy = SpyStore()
    ledger = ledger_with(spy)
    spy.inner.lock("h", "old", 60)
    spy.fail("unlock", "raise")
    ledger.unlock("h", "old", 60)
    assert ledger.lock("h", "new", 60) is True  # the payer's retry is not held at "in progress"
    assert spy.inner.lock_holder("h") == "new"


def test_a_given_up_claim_stays_pending_until_the_store_answers_or_its_ttl_ends() -> None:
    clock = Clock()
    spy = SpyStore()
    ledger = Ledger(spy, logging.getLogger("test-ledger"), clock=clock)
    spy.inner.claim("u", "tok", 3600)
    spy.fail("unclaim", "raise")
    ledger.unclaim("u", "tok")
    spy.fail("unclaim", "raise", "raise")
    ledger.reconcile_once()
    ledger.reconcile_once()
    assert ledger.left_behind()["claims"] == 1  # still pending: the store has not answered
    ledger.reconcile_once()
    assert ledger.left_behind()["claims"] == 0 and spy.inner.claimant("u") is None
    spy.inner.claim("v", "tok", 3600)
    spy.fail("unclaim", "raise")
    ledger.unclaim("v", "tok")
    clock.now += 27 * 3600  # the claim's own TTL ended
    ledger.reconcile_once()
    assert ledger.left_behind()["claims"] == 0


def test_fallback_only_records_reach_the_store_through_the_reconciler() -> None:
    spy = SpyStore()
    ledger = ledger_with(spy)
    spy.down = {"put_record"}
    with pytest.raises(ConnectionError):
        ledger.save_settled("h", Record(STATE_SETTLED, "d", "u"))
    assert spy.inner.record("h") is None and ledger.left_behind()["records"] == 1
    ledger.reconcile_once()  # the store still refuses: kept
    assert ledger.left_behind()["records"] == 1
    spy.up()
    ledger.reconcile_once()
    assert (
        spy.inner.record("h") == Record(STATE_SETTLED, "d", "u")
        and ledger.left_behind()["records"] == 0
    )


def test_the_reconciler_never_writes_over_a_record_that_changed_since_its_snapshot() -> None:
    spy = SpyStore()
    ledger = ledger_with(spy)
    spy.down = {"put_record"}
    with pytest.raises(ConnectionError):
        ledger.save_attempt("h", Record(STATE_ATTEMPT, "d"))
    spy.up()
    spy.inner.put_record(
        "h", Record(STATE_SETTLED, "d", "u"), 60
    )  # settled by another instance meanwhile
    ledger.reconcile_once()
    assert spy.inner.record("h") == Record(
        STATE_SETTLED, "d", "u"
    )  # an attempt never replaces a settled record


def test_the_reconciler_skips_a_payment_a_request_holds() -> None:
    spy = SpyStore()
    ledger = ledger_with(spy)
    spy.down = {"put_record"}
    with pytest.raises(ConnectionError):
        ledger.save_settled("h", Record(STATE_SETTLED, "d", "u"))
    spy.up()
    spy.inner.lock("h", "request", 60)  # a request is working on the payment
    ledger.reconcile_once()
    assert spy.inner.record("h") is None and ledger.left_behind()["records"] == 1
    spy.inner.unlock("h", "request")
    ledger.reconcile_once()
    assert spy.inner.record("h") is not None


def test_a_failing_store_never_kills_the_reconciler_thread() -> None:
    import threading
    import time

    from ftptech_x402_canton.reconciler import Reconciler

    spy = SpyStore()
    ledger = ledger_with(spy)
    spy.inner.lock("h", "a", 60)
    spy.fail("unlock", "raise")
    ledger.unlock("h", "a", 60)

    def boom() -> None:
        raise RuntimeError("store panic")

    reconciler = Reconciler(ledger, 0.02, logging.getLogger("test-ledger"))
    original = ledger.reconcile_once
    calls: list[int] = []

    def flaky(final: bool = False) -> None:
        calls.append(1)
        if len(calls) == 1:
            boom()
        original()

    ledger.reconcile_once = flaky  # type: ignore[method-assign]
    reconciler.ensure_started()
    deadline = time.monotonic() + 5
    while ledger.left_behind()["locks"] and time.monotonic() < deadline:
        time.sleep(0.02)
    assert ledger.left_behind()["locks"] == 0 and len(calls) >= 2
    reconciler.stop()
    assert not any(t.name == "canton-reconciler" and t.is_alive() for t in threading.enumerate())


def test_stop_runs_a_final_pass_and_logs_what_is_left(caplog: pytest.LogCaptureFixture) -> None:
    from ftptech_x402_canton.reconciler import Reconciler

    spy = SpyStore()
    ledger = ledger_with(spy)
    spy.inner.claim("u", "tok", 60)
    spy.down = {"unclaim"}
    ledger.unclaim("u", "tok")
    reconciler = Reconciler(ledger, 60, logging.getLogger("test-ledger"))
    reconciler.stop()
    assert any("stopped with work left" in r.getMessage() for r in caplog.records)
    spy.up()
    reconciler2 = Reconciler(ledger, 60, logging.getLogger("test-ledger"))
    reconciler2.stop()  # the final pass releases it
    assert spy.inner.claimant("u") is None


def test_save_settled_keeps_a_settled_record_of_the_same_update_id_and_replaces_an_attempt() -> (
    None
):
    spy = SpyStore()
    ledger = ledger_with(spy)
    ledger.save_attempt("h", Record(STATE_ATTEMPT, "digest-A"))
    ledger.save_settled(
        "h", Record(STATE_SETTLED, "digest-B", "u1")
    )  # another /supported pin, other bytes
    assert spy.inner.record("h") == Record(STATE_SETTLED, "digest-B", "u1")
    ledger.save_settled(
        "h", Record(STATE_SETTLED, "digest-C", "u1")
    )  # same updateId: kept as it is
    assert spy.inner.record("h") == Record(STATE_SETTLED, "digest-B", "u1")
    with pytest.raises(RecordConflictError):
        ledger.save_settled("h", Record(STATE_SETTLED, "digest-C", "u2"))


def test_a_full_memory_store_admits_the_retry_of_a_recorded_payment() -> None:
    store = memory(max_entries=3)
    assert store.lock("h", "t", 60) is True
    store.put_record("h", Record(STATE_SETTLED, "d", "u"), RECORD_TTL_SECONDS)
    store.claim("u", "t", RECORD_TTL_SECONDS)
    store.unlock("h", "t")
    assert len(store) == 2  # type: ignore[arg-type]
    store.claim("x", "t", RECORD_TTL_SECONDS)  # the store is now full of live entries
    with pytest.raises(StoreFullError):
        store.lock("fresh", "t2", 60)  # a new payment is refused ...
    assert store.lock("h", "t3", 60) is True  # ... the retry of a recorded one is not


def test_record_json_keeps_the_attempt_time() -> None:
    record = Record(STATE_ATTEMPT, "d", at=1_700_000_000)
    assert Record.from_json(record.to_json()) == record
    assert (
        Record.from_json('{"state":"attempt","digest":"d"}').at == 0
    )  # records written before the field
