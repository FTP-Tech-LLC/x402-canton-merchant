"""A Redis-backed store that several instances can share."""

from __future__ import annotations

import math
import os
import uuid
from typing import Any, Protocol

from .errors import CantonConfigError
from .store import Record, delivered_mark
from .store_memory import KEY_CLAIM, KEY_LOCK, KEY_RECORD

#: Used when an empty prefix is given, so the plugin's keys never land bare in a
#: shared Redis database.
DEFAULT_REDIS_PREFIX = "x402-canton:"

#: Seconds. A redis-py call that waits longer fails. One store call on a new connection can take
#: about 8 round trips (connect, the handshake commands, the command), each up to its timeout:
#: 1 s keeps a call under the plugin's per-call store budget (``STORE_CALL_BUDGET``, 10 s).
DEFAULT_SOCKET_TIMEOUT_SECONDS = 1.0
DEFAULT_CONNECT_TIMEOUT_SECONDS = 1.0
#: redis-py 8 caps a pool at 100 connections by default and fails a call at once beyond it. The
#: plugin runs up to ``worker_threads`` (64) paid requests at a time, plus the reconciler.
DEFAULT_MAX_CONNECTIONS = 256
_PROBE_TTL_SECONDS = 30
#: Hash tags whose CRC16 slot is ``i * 1024`` for i in 0..15: probe keys ``{tag}`` land in 16 slots
#: spread over the 16384, so a Redis Cluster node answers MOVED for most of them.
_SLOT_TAGS = (
    "s7920", "s28308", "s10209", "s3175", "s45693", "s1683", "s12132", "s66936",
    "s115497", "s23461", "s3313", "s42471", "s4397", "s12799", "s1028", "s12354",
)  # fmt: skip
#: Round trips of a call on a new connection (connect, handshake commands, the command), each up to
#: its timeout. A TLS handshake must also fit inside the connect timeout.
_ROUND_TRIPS_PER_CALL = 8


def _is_redirect(error: Exception) -> bool:
    return type(error).__name__ in ("MovedError", "AskError") or str(error).startswith(
        ("MOVED", "ASK")
    )


def _cluster_error() -> CantonConfigError:
    return CantonConfigError(
        [
            "the Redis endpoint is a Redis Cluster: the store's keys would hash to different "
            "slots; use a single Redis (no Redis Cluster endpoint)"
        ]
    )


_DELETE_IF_VALUE_LUA = (
    'if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) end return 0'
)


class RedisClient(Protocol):
    """The small surface the Redis store needs. No redis-py dependency: see :class:`RedisPyClient`.

    Optional: a ``check()`` method that raises when the server cannot serve the store (a Redis
    Cluster endpoint, a read-only replica, no Lua). ``CantonServer.start`` calls it."""

    def set_nx(self, key: str, value: str, ttl_seconds: int) -> bool:
        """``SET key value NX EX ttl``. False when the key exists."""
        ...

    def set(self, key: str, value: str, ttl_seconds: int) -> None:
        """``SET key value EX ttl``: overwrites."""
        ...

    def get(self, key: str) -> str | None:
        """The value, or None when the key does not exist."""
        ...

    def delete(self, key: str) -> None: ...

    def delete_if_value(self, key: str, value: str) -> None:
        """Delete the key only when its value equals ``value``, atomically (a Lua compare-and-delete)."""
        ...


class RedisPyClient:
    """:class:`RedisClient` over a redis-py client (bytes or decoded responses both work).

    Build the client with client retries off (``retry=Retry(NoBackoff(), 0)``) and a
    ``socket_timeout``: redis-py 6 and newer re-send a command after a lost reply by default, and a
    ``SET NX`` then answers "taken" for a lock this process holds. :meth:`RedisStore.from_url` does it.
    """

    def __init__(self, client: Any) -> None:
        self._client = client
        self._delete_if = client.register_script(_DELETE_IF_VALUE_LUA)

    def check(self, prefix: str = DEFAULT_REDIS_PREFIX) -> None:
        """Raise unless this is a writable, single Redis with Lua, using only keys under ``prefix``.

        No PING and no required INFO: an ACL user may lack them, and a proxy may answer them oddly.
        ``INFO cluster`` is only a hint (``cluster_enabled:1`` raises; any failure or odd reply is
        "unknown"). The real test is reads of 16 keys spread over the 16384 hash slots in one
        pipeline: a Redis Cluster node answers MOVED or ASK for most of them. Then one write probe:
        set, read and compare-and-delete a key under the prefix.
        """
        self._refuse_cluster_by_info()
        self._refuse_cluster_by_slots(prefix)
        probe, value = f"{prefix}probe:{os.getpid()}:{uuid.uuid4().hex}", uuid.uuid4().hex
        if not self.set_nx(probe, value, _PROBE_TTL_SECONDS) or self.get(probe) != value:
            raise RuntimeError("the Redis store probe key could not be written and read back")
        self.delete_if_value(probe, value)
        if self.get(probe) is not None:
            raise RuntimeError("the Redis compare-and-delete (Lua) did not delete the probe key")

    def _refuse_cluster_by_info(self) -> None:
        try:
            info = self._client.info("cluster")
            enabled = int(info.get("cluster_enabled", 0))
        except Exception:  # noqa: BLE001 - denied, disabled, a list reply from a proxy: unknown
            return
        if enabled:
            raise _cluster_error()

    def _refuse_cluster_by_slots(self, prefix: str) -> None:
        pipeline = self._client.pipeline(transaction=False)
        for tag in _SLOT_TAGS:
            pipeline.get(f"{prefix}{{{tag}}}probe")
        try:
            results = pipeline.execute(raise_on_error=False)
        except Exception as error:  # noqa: BLE001 - some redis-py versions raise MOVED from execute
            if _is_redirect(error):
                raise _cluster_error() from error
            raise
        for result in results:
            if isinstance(result, Exception):
                if _is_redirect(result):
                    raise _cluster_error()
                raise result

    @property
    def call_budget_seconds(self) -> float:
        """The longest one store call can take on this client: about 8 round trips on a new
        connection, each up to the larger of its socket and connect timeouts. 0 when unknown."""
        try:
            options = self._client.connection_pool.connection_kwargs
            waits = [options.get("socket_timeout"), options.get("socket_connect_timeout")]
            longest = max(float(w) for w in waits if w is not None)
        except (AttributeError, ValueError, TypeError):
            return 0.0
        return _ROUND_TRIPS_PER_CALL * longest

    def set_nx(self, key: str, value: str, ttl_seconds: int) -> bool:
        return bool(self._client.set(key, value, nx=True, ex=ttl_seconds))

    def set(self, key: str, value: str, ttl_seconds: int) -> None:
        self._client.set(key, value, ex=ttl_seconds)

    def get(self, key: str) -> str | None:
        value = self._client.get(key)
        # Independent of the client's decode setting: a bytes client gives the same answers.
        return value.decode() if isinstance(value, bytes) else value

    def delete(self, key: str) -> None:
        self._client.delete(key)

    def delete_if_value(self, key: str, value: str) -> None:
        self._delete_if(keys=[key], args=[value])


class RedisStore:
    """:class:`~ftptech_x402_canton.store.Store` on Redis.

    Locks and claims use ``SET NX EX`` with the owner's token, records a plain
    ``SET EX``. Every key starts with ``prefix`` (for example ``x402:myapp:``).
    Use a dedicated database with ``maxmemory-policy noeviction`` and read from
    the primary: a replica or an evicting policy can hide a delivered claim.
    """

    def __init__(self, client: RedisClient, prefix: str = "") -> None:
        self._client = client
        self._prefix = prefix or DEFAULT_REDIS_PREFIX

    @classmethod
    def from_url(cls, url: str, *, prefix: str, **client_options: Any) -> RedisStore:
        """A store on the Redis at ``url``, with the settings the plugin needs.

        The client has client retries off, a 1 s socket and connect timeout, decoded
        responses and a pool of 256 connections; ``client_options`` go to ``redis.Redis.from_url``
        and override them. For a remote or TLS Redis raise ``socket_timeout`` and
        ``socket_connect_timeout`` (the connect timeout covers the whole TLS handshake); the
        request budget and the lock TTL follow them. Needs redis-py: ``pip install "ftptech-x402-canton[redis]"``. A single
        Redis only, not a Redis Cluster endpoint (``CantonServer.start`` refuses one).
        ``prefix`` is required and must not be empty, so the keyspace is chosen on purpose:
        services with the same ``pay_to`` and terms must share one prefix.
        """
        if not prefix:
            raise CantonConfigError(["RedisStore.from_url: prefix must not be empty"])
        try:
            import redis
            from redis.backoff import NoBackoff
            from redis.retry import Retry
        except ImportError as error:
            raise CantonConfigError(
                ['RedisStore.from_url needs redis-py: pip install "ftptech-x402-canton[redis]"']
            ) from error
        options: dict[str, Any] = {
            "decode_responses": True,
            "socket_timeout": DEFAULT_SOCKET_TIMEOUT_SECONDS,
            "socket_connect_timeout": DEFAULT_CONNECT_TIMEOUT_SECONDS,
            "max_connections": DEFAULT_MAX_CONNECTIONS,
            "retry": Retry(NoBackoff(), 0),
            **client_options,
        }
        return cls(RedisPyClient(redis.Redis.from_url(url, **options)), prefix)

    def check(self) -> None:
        """Called by ``CantonServer.start``: does the client's own check, when it has one."""
        check = getattr(self._client, "check", None)
        if check is not None:
            check(self._prefix)

    @property
    def call_budget_seconds(self) -> float:
        """What the client's timeouts allow one store call (0 when the client does not say)."""
        return float(getattr(self._client, "call_budget_seconds", 0.0))

    def _key(self, kind: str, name: str) -> str:
        return f"{self._prefix}{kind}{name}"

    def lock(self, hash: str, token: str, ttl: float) -> bool:
        return self._client.set_nx(self._key(KEY_LOCK, hash), token, _seconds(ttl))

    def lock_holder(self, hash: str) -> str | None:
        return self._client.get(self._key(KEY_LOCK, hash))

    def unlock(self, hash: str, token: str) -> None:
        self._client.delete_if_value(self._key(KEY_LOCK, hash), token)

    def record(self, hash: str) -> Record | None:
        value = self._client.get(self._key(KEY_RECORD, hash))
        return None if value is None else Record.from_json(value)

    def put_record(self, hash: str, record: Record, ttl: float) -> None:
        self._client.set(self._key(KEY_RECORD, hash), record.to_json(), _seconds(ttl))

    def delete_record(self, hash: str) -> None:
        self._client.delete(self._key(KEY_RECORD, hash))

    def claim(self, update_id: str, token: str, ttl: float) -> bool:
        return self._client.set_nx(self._key(KEY_CLAIM, update_id), token, _seconds(ttl))

    def claimant(self, update_id: str) -> str | None:
        return self._client.get(self._key(KEY_CLAIM, update_id))

    def unclaim(self, update_id: str, token: str) -> None:
        self._client.delete_if_value(self._key(KEY_CLAIM, update_id), token)

    def mark_delivered(self, update_id: str, token: str, ttl: float) -> None:
        self._client.set(self._key(KEY_CLAIM, update_id), delivered_mark(token), _seconds(ttl))


def _seconds(ttl: float) -> int:
    """Redis EX takes whole seconds, at least 1. Round up so a TTL is never shortened."""
    return max(1, math.ceil(ttl))
