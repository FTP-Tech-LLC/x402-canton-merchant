"""RedisStore.from_url and the redis-py adapter: fakeredis (Lua for compare-and-delete), and a live
Redis when REDIS_URL is set."""

from __future__ import annotations

import os
import sys
import uuid
from typing import Any

import pytest

from ftptech_x402_canton import CantonConfigError, MemoryStore, RedisStore
from ftptech_x402_canton.store import STATE_SETTLED, Record

fakeredis = pytest.importorskip("fakeredis")
redis = pytest.importorskip("redis")


def patched_from_url(monkeypatch: pytest.MonkeyPatch, server: Any = None) -> dict[str, Any]:
    """Make redis.Redis.from_url return a fakeredis client; record the options it was given."""
    seen: dict[str, Any] = {}
    shared = server or fakeredis.FakeServer()

    def from_url(url: str, **options: Any) -> Any:
        seen["url"], seen["options"] = url, options
        return fakeredis.FakeRedis(
            server=shared,
            decode_responses=options["decode_responses"],
            socket_timeout=options.get("socket_timeout"),
            socket_connect_timeout=options.get("socket_connect_timeout"),
        )

    monkeypatch.setattr(redis.Redis, "from_url", staticmethod(from_url))
    return seen


def test_from_url_builds_a_client_with_retries_off_and_timeouts(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    seen = patched_from_url(monkeypatch)
    RedisStore.from_url("redis://cache:6379/3", prefix="x402:app:")
    options = seen["options"]
    assert seen["url"] == "redis://cache:6379/3"
    assert options["decode_responses"] is True
    assert (
        getattr(options["retry"], "_retries", None) == 0
    )  # a lost reply is never re-sent by the client
    assert options["socket_timeout"] > 0 and options["socket_connect_timeout"] > 0


def test_from_url_options_override_the_defaults(monkeypatch: pytest.MonkeyPatch) -> None:
    seen = patched_from_url(monkeypatch)
    RedisStore.from_url("redis://x", prefix="p:", socket_timeout=2, health_check_interval=30)
    assert seen["options"]["socket_timeout"] == 2
    assert seen["options"]["health_check_interval"] == 30


def test_from_url_requires_a_prefix() -> None:
    with pytest.raises(CantonConfigError, match="prefix"):
        RedisStore.from_url("redis://x", prefix="")


def test_the_store_works_end_to_end_on_redis_py(monkeypatch: pytest.MonkeyPatch) -> None:
    patched_from_url(monkeypatch)
    store = RedisStore.from_url("redis://x", prefix="x402:t:")
    assert store.lock("h", "tok1", 60) and not store.lock("h", "tok2", 60)
    store.unlock("h", "other")  # a foreign token deletes nothing (the Lua compare-and-delete)
    assert store.lock_holder("h") == "tok1"
    store.unlock("h", "tok1")
    assert store.lock_holder("h") is None
    assert store.claim("u", "c1", 60) and not store.claim("u", "c2", 60)
    store.unclaim("u", "c2")
    assert store.claimant("u") == "c1"
    store.unclaim("u", "c1")
    assert store.claimant("u") is None
    record = Record(STATE_SETTLED, "digest", "update-1", "payer")
    store.put_record("h", record, 3600)
    assert store.record("h") == record
    store.delete_record("h")
    assert store.record("h") is None


def test_two_stores_on_one_server_share_state(monkeypatch: pytest.MonkeyPatch) -> None:
    server = fakeredis.FakeServer()
    patched_from_url(monkeypatch, server)
    first = RedisStore.from_url("redis://x", prefix="x402:t:")
    second = RedisStore.from_url("redis://x", prefix="x402:t:")
    assert first.claim("u", "c1", 60) and not second.claim("u", "c2", 60)


def test_from_url_without_redis_py_names_the_extra(monkeypatch: pytest.MonkeyPatch) -> None:
    for name in [m for m in sys.modules if m == "redis" or m.startswith("redis.")]:
        monkeypatch.setitem(sys.modules, name, None)  # import redis now raises ImportError
    with pytest.raises(CantonConfigError, match=r"ftptech-x402-canton\[redis\]"):
        RedisStore.from_url("redis://x", prefix="p:")


@pytest.mark.skipif(not os.environ.get("REDIS_URL"), reason="REDIS_URL not set")
def test_the_store_works_on_a_live_redis() -> None:
    prefix = f"x402-test:{uuid.uuid4().hex}:"
    store = RedisStore.from_url(os.environ["REDIS_URL"], prefix=prefix)
    assert store.lock("h", "tok1", 30) and not store.lock("h", "tok2", 30)
    store.unlock("h", "tok2")
    assert store.lock_holder("h") == "tok1"
    store.unlock("h", "tok1")
    assert store.lock_holder("h") is None
    record = Record(STATE_SETTLED, "digest", "update-1", "payer")
    store.put_record("h", record, 30)
    assert store.record("h") == record
    store.delete_record("h")
    assert store.claim("u", "c1", 30) and not store.claim("u", "c2", 30)
    store.unclaim("u", "c1")


# -- the adapter does not depend on the client's decode setting; startup check ------------------------------------------


def test_a_bytes_client_gives_the_same_answers_as_a_decoded_one() -> None:
    from ftptech_x402_canton import RedisPyClient

    client = fakeredis.FakeRedis(decode_responses=False)
    store = RedisStore(RedisPyClient(client), "x402:t:")
    assert store.lock("h", "tok1", 60) and not store.lock("h", "tok2", 60)
    assert store.lock_holder("h") == "tok1"  # a str, not bytes: own-lock recognition works
    assert store.claim("u", "c1", 60)
    store.mark_delivered("u", "c1", 60)
    assert store.claimant("u") == "delivered:c1"
    record = Record(STATE_SETTLED, "digest", "update-1", "payer")
    store.put_record("h", record, 3600)
    assert store.record("h") == record


def test_from_url_with_decode_responses_false_still_works(monkeypatch: pytest.MonkeyPatch) -> None:
    patched_from_url(monkeypatch)
    store = RedisStore.from_url("redis://x", prefix="p:", decode_responses=False)
    assert store.claim("u", "c1", 60) and store.claimant("u") == "c1"


def test_a_redis_cluster_endpoint_is_refused_at_startup(
    monkeypatch: pytest.MonkeyPatch, facilitator: Any
) -> None:
    from ftptech_x402_canton import RedisPyClient
    from tests import inproc

    client = fakeredis.FakeRedis(decode_responses=True)
    monkeypatch.setattr(client, "info", lambda section=None: {"cluster_enabled": 1})
    server = inproc.build_server(
        facilitator, start=False, store=RedisStore(RedisPyClient(client), "p:")
    )
    with pytest.raises(CantonConfigError, match="Redis Cluster"):
        server.start()


def test_the_startup_check_fails_on_a_store_that_cannot_write(
    monkeypatch: pytest.MonkeyPatch, facilitator: Any
) -> None:
    from ftptech_x402_canton import CantonStartupError, RedisPyClient
    from tests import inproc

    client = fakeredis.FakeRedis(decode_responses=True)

    def readonly(*args: Any, **kwargs: Any) -> Any:
        raise redis.exceptions.ReadOnlyError(
            "READONLY You can't write against a read only replica."
        )

    monkeypatch.setattr(client, "set", readonly)
    server = inproc.build_server(
        facilitator, start=False, store=RedisStore(RedisPyClient(client), "p:")
    )
    with pytest.raises(CantonStartupError, match="payment store unusable"):
        server.start()


def test_the_startup_check_passes_on_a_working_redis(facilitator: Any) -> None:
    from ftptech_x402_canton import RedisPyClient
    from tests import inproc

    client = fakeredis.FakeRedis(decode_responses=True)
    server = inproc.build_server(facilitator, store=RedisStore(RedisPyClient(client), "p:"))
    assert server.ready
    assert not [
        key for key in client.keys() if b"probe" in key or "probe" in str(key)
    ]  # probe removed
    server.close()


def test_from_url_defaults_fit_the_store_budget() -> None:
    from ftptech_x402_canton.config import STORE_CALL_BUDGET
    from ftptech_x402_canton.store_redis import (
        DEFAULT_CONNECT_TIMEOUT_SECONDS,
        DEFAULT_MAX_CONNECTIONS,
        DEFAULT_SOCKET_TIMEOUT_SECONDS,
    )

    # A call on a new connection is about 8 round trips (connect, handshake, command).
    assert (
        8 * max(DEFAULT_SOCKET_TIMEOUT_SECONDS, DEFAULT_CONNECT_TIMEOUT_SECONDS)
        <= STORE_CALL_BUDGET
    )
    assert DEFAULT_MAX_CONNECTIONS >= 64 + 64  # worker_threads (64) paid requests, plus a margin


def test_the_reconciler_recognizes_its_own_lock_when_the_client_re_sent_the_set_nx() -> None:
    import logging

    from ftptech_x402_canton import MemoryStore
    from ftptech_x402_canton.ledger import Ledger

    class ResendingStore(MemoryStore):
        """lock() applies the lock but answers False: a client that re-sent the SET NX after a lost reply."""

        def lock(self, hash: str, token: str, ttl: float) -> bool:
            super().lock(hash, token, ttl)
            return False

    store = ResendingStore()
    ledger = Ledger(store, logging.getLogger("t"))
    record = Record(STATE_SETTLED, "digest", "update-1", "payer")
    ledger._to_fallback("h", record)  # noqa: SLF001
    ledger.reconcile_once(final=True)
    assert store.record("h") == record  # written, not skipped as "busy"


@pytest.mark.skipif(not os.environ.get("REDIS_URL"), reason="REDIS_URL not set")
def test_the_startup_check_and_a_bytes_client_work_on_a_live_redis(facilitator: Any) -> None:
    from ftptech_x402_canton import RedisPyClient
    from tests import inproc

    url = os.environ["REDIS_URL"]
    prefix = f"x402-test:{uuid.uuid4().hex}:"
    raw = redis.Redis.from_url(url, decode_responses=False)
    store = RedisStore(RedisPyClient(raw), prefix)
    store.check()  # a writable, non-cluster Redis with Lua
    assert store.lock("h", "tok1", 30) and store.lock_holder("h") == "tok1"
    store.unlock("h", "tok1")
    server = inproc.build_server(facilitator, store=RedisStore.from_url(url, prefix=prefix))
    assert server.ready
    server.close()


# -- round 6: the check needs no PING or INFO, spreads probes over the slots, retries, budget follows timeouts -----------------


import socketserver  # noqa: E402
import threading  # noqa: E402

from ftptech_x402_canton import RedisPyClient  # noqa: E402
from tests import inproc  # noqa: E402

SLOT_TAGS_MOVED_ABOVE = 8192


def crc16_slot(key: str) -> int:
    inner = key
    start = key.find("{")
    if start != -1 and key.find("}", start + 1) > start + 1:
        inner = key[start + 1 : key.find("}", start + 1)]
    crc = 0
    for byte in inner.encode():
        crc ^= byte << 8
        for _ in range(8):
            crc = ((crc << 1) ^ 0x1021) & 0xFFFF if crc & 0x8000 else (crc << 1) & 0xFFFF
    return crc % 16384


def test_the_slot_tags_cover_the_hash_space() -> None:
    from ftptech_x402_canton.store_redis import _SLOT_TAGS

    assert sorted(crc16_slot("{" + tag + "}") for tag in _SLOT_TAGS) == [
        i * 1024 for i in range(16)
    ]


class ScriptedRedis(socketserver.ThreadingTCPServer):
    """A RESP endpoint that answers MOVED for keys whose slot is >= 8192, like a cluster node, and
    answers INFO as ``info_mode`` says: "error" (denied), "list" (a proxy's array reply) or "plain"."""

    allow_reuse_address = True
    daemon_threads = True

    def __init__(self, info_mode: str) -> None:
        self.info_mode = info_mode
        self.data: dict[str, str] = {}
        super().__init__(("127.0.0.1", 0), ScriptedHandler)

    @property
    def port(self) -> int:
        return int(self.server_address[1])


class ScriptedHandler(socketserver.StreamRequestHandler):
    def read_command(self) -> list[str] | None:
        line = self.rfile.readline()
        if not line:
            return None
        count = int(line[1:])
        parts = []
        for _ in range(count):
            size = int(self.rfile.readline()[1:])
            parts.append(self.rfile.read(size + 2)[:-2].decode())
        return parts

    def handle(self) -> None:
        server: ScriptedRedis = self.server  # type: ignore[assignment]
        while (command := self.read_command()) is not None:
            name = command[0].upper()
            if name in ("GET", "SET") and crc16_slot(command[1]) >= SLOT_TAGS_MOVED_ABOVE:
                self.wfile.write(f"-MOVED {crc16_slot(command[1])} 127.0.0.1:7001\r\n".encode())
            elif name == "GET":
                value = server.data.get(command[1])
                self.wfile.write(
                    b"$-1\r\n" if value is None else f"${len(value)}\r\n{value}\r\n".encode()
                )
            elif name == "SET":
                server.data[command[1]] = command[2]
                self.wfile.write(b"+OK\r\n")
            elif name == "INFO":
                if server.info_mode == "error":
                    self.wfile.write(
                        b"-NOPERM this user has no permissions to run the 'info' command\r\n"
                    )
                elif server.info_mode == "list":
                    self.wfile.write(b"*2\r\n$7\r\ncluster\r\n$1\r\n1\r\n")
                else:
                    self.wfile.write(b"$18\r\n# Cluster\r\r\n\r\n")
            else:
                self.wfile.write(b"+OK\r\n")
            self.wfile.flush()


@pytest.mark.parametrize("info_mode", ["error", "list", "plain"])
def test_a_cluster_node_is_refused_by_its_moved_answers_whatever_info_says(info_mode: str) -> None:
    server = ScriptedRedis(info_mode)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        store = RedisStore.from_url(
            f"redis://127.0.0.1:{server.port}", prefix="x402:t:", protocol=2
        )
        with pytest.raises(CantonConfigError, match="Redis Cluster"):
            store.check()
    finally:
        server.shutdown()
        server.server_close()


def acl_user_client(monkeypatch: pytest.MonkeyPatch, rules: str, pattern: str) -> tuple[Any, Any]:
    """A live Redis user with the given ACL rules, and the admin client that removes it later."""
    url = os.environ["REDIS_URL"]
    admin = redis.Redis.from_url(url, decode_responses=True)
    name = f"x402test{uuid.uuid4().hex[:8]}"
    admin.execute_command(
        "ACL", "SETUSER", name, "on", ">pw", f"~{pattern}", "+@all", *rules.split()
    )
    options = redis.connection.parse_url(url)
    host, port = options["host"], options["port"]
    db = options.get("db", 0)
    user = redis.Redis(
        host=host, port=port, db=db, username=name, password="pw", decode_responses=True
    )
    return user, (admin, name)


@pytest.mark.skipif(not os.environ.get("REDIS_URL"), reason="REDIS_URL not set")
@pytest.mark.parametrize(
    "rules",
    ["-@dangerous", "-info", "-ping", "-@dangerous -ping -info"],
)
def test_the_check_passes_for_acl_users_without_info_or_ping_and_a_key_pattern(
    monkeypatch: pytest.MonkeyPatch, rules: str
) -> None:
    prefix = f"x402:acl{uuid.uuid4().hex[:6]}:"
    user, (admin, name) = acl_user_client(monkeypatch, rules, prefix + "*")
    try:
        RedisStore(RedisPyClient(user), prefix).check()  # keys only under the prefix
    finally:
        admin.execute_command("ACL", "DELUSER", name)


def test_the_check_treats_a_failing_or_odd_info_as_unknown() -> None:
    class Odd(fakeredis.FakeRedis):  # type: ignore[name-defined,misc]
        mode = "error"

        def info(self, section: Any = None, *args: Any, **kwargs: Any) -> Any:
            if self.mode == "error":
                raise redis.exceptions.NoPermissionError("NOPERM info")
            if self.mode == "other":
                raise ConnectionError("boom")
            return ["cluster", "x"]  # a proxy's list reply

        def ping(self, *args: Any, **kwargs: Any) -> Any:
            raise redis.exceptions.NoPermissionError("NOPERM ping")

    for mode in ("error", "other", "list"):
        client = Odd(decode_responses=True)
        client.mode = mode
        RedisStore(RedisPyClient(client), "x402:t:").check()  # no startup error, no PING needed


def test_the_probe_keys_are_all_under_the_prefix() -> None:
    client = fakeredis.FakeRedis(decode_responses=True)
    seen: list[str] = []
    original = client.execute_command

    def spy(*args: Any, **kwargs: Any) -> Any:
        seen.append(str(args[1]) if len(args) > 1 else "")
        return original(*args, **kwargs)

    client.execute_command = spy
    RedisStore(RedisPyClient(client), "x402:svc:").check()
    probes = [key for key in seen if "probe" in key]
    assert probes and all(key.startswith("x402:svc:") for key in probes)


class SlowStartStore(MemoryStore):
    """A store whose check fails ``failures`` times (Redis not listening yet), then passes."""

    def __init__(self, failures: int) -> None:
        super().__init__()
        self.failures = failures
        self.tries = 0

    def check(self) -> None:
        self.tries += 1
        if self.tries <= self.failures:
            raise ConnectionError("Connection refused")


def test_the_store_check_is_retried_within_supported_wait(facilitator: Any) -> None:
    store = SlowStartStore(failures=3)
    server = inproc.build_server(facilitator, store=store)
    assert server.ready and store.tries == 4
    server.close()


def test_a_store_that_stays_down_fails_the_start_after_the_wait_and_names_the_store(
    facilitator: Any,
) -> None:
    from ftptech_x402_canton import CantonStartupError

    store = SlowStartStore(failures=10**6)
    server = inproc.build_server(facilitator, start=False, store=store, supported_wait=0.5)
    with pytest.raises(
        CantonStartupError, match=r"payment store unusable after 0\.5s: .*Connection refused"
    ):
        server.start()
    assert store.tries > 1  # retried, not one shot


def test_the_request_budget_follows_the_clients_timeouts() -> None:
    from ftptech_x402_canton import Config, Route, cc, request_budget
    from tests.support import PAY_TO

    routes = {"GET /x": Route(pay_to=PAY_TO, price=cc("1"))}  # a real client: nothing connects
    defaults = RedisStore.from_url("redis://x", prefix="p:")
    assert (
        request_budget(Config(routes=routes, store=defaults)) == 465
    )  # the defaults do not change it
    slow = RedisStore.from_url("redis://x", prefix="p:", socket_timeout=3, socket_connect_timeout=5)
    assert slow.call_budget_seconds == 8 * 5
    assert request_budget(Config(routes=routes, store=slow)) == 465 + 7 * (40 - 10)
