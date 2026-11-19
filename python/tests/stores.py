"""Store test doubles: a spy, and failure injection (down, errors, lost replies)."""

from __future__ import annotations

from collections import deque
from typing import Any

from ftptech_x402_canton import MemoryStore, Record

METHODS = (
    "lock", "lock_holder", "unlock", "record", "put_record", "delete_record", "claim", "claimant",
    "unclaim", "mark_delivered",
)  # fmt: skip


class SpyStore:
    """Wraps a store; records every call; scripted failures per method.

    Script entries, consumed one per call: ``"raise"`` (fail before applying),
    ``"apply_raise"`` (apply, then fail: a lost reply) or ``"ok"``.
    """

    non_blocking = True

    def __init__(self, inner: Any = None) -> None:
        self.inner = inner if inner is not None else MemoryStore()
        self.calls: list[tuple[str, tuple[Any, ...]]] = []
        self.script: dict[str, deque[str]] = {m: deque() for m in METHODS}
        self.down: set[str] = set()  # methods that always raise

    def fail(self, method: str, *modes: str) -> None:
        self.script[method].extend(modes)

    def down_all(self) -> None:
        self.down = set(METHODS)

    def up(self) -> None:
        self.down = set()

    def names(self) -> list[str]:
        return [name for name, _ in self.calls]

    def args(self, method: str) -> list[tuple[Any, ...]]:
        return [a for name, a in self.calls if name == method]

    def _run(self, method: str, *args: Any) -> Any:
        self.calls.append((method, args))
        if method in self.down:
            raise ConnectionError(f"store down: {method}")
        mode = self.script[method].popleft() if self.script[method] else "ok"
        if mode == "raise":
            raise ConnectionError(f"store error: {method}")
        result = getattr(self.inner, method)(*args)
        if mode == "apply_raise":
            raise ConnectionError(f"store lost reply: {method}")
        return result

    def lock(self, hash: str, token: str, ttl: float) -> bool:
        return bool(self._run("lock", hash, token, ttl))

    def lock_holder(self, hash: str) -> str | None:
        holder: str | None = self._run("lock_holder", hash)
        return holder

    def unlock(self, hash: str, token: str) -> None:
        self._run("unlock", hash, token)

    def record(self, hash: str) -> Record | None:
        record: Record | None = self._run("record", hash)
        return record

    def put_record(self, hash: str, record: Record, ttl: float) -> None:
        self._run("put_record", hash, record, ttl)

    def delete_record(self, hash: str) -> None:
        self._run("delete_record", hash)

    def claim(self, update_id: str, token: str, ttl: float) -> bool:
        return bool(self._run("claim", update_id, token, ttl))

    def claimant(self, update_id: str) -> str | None:
        holder: str | None = self._run("claimant", update_id)
        return holder

    def unclaim(self, update_id: str, token: str) -> None:
        self._run("unclaim", update_id, token)

    def mark_delivered(self, update_id: str, token: str, ttl: float) -> None:
        self._run("mark_delivered", update_id, token, ttl)


class FakeRedis:
    """An in-memory ``RedisClient`` with TTLs recorded, and Lua-style compare-and-delete."""

    def __init__(self) -> None:
        self.data: dict[str, str] = {}
        self.ttls: dict[str, int] = {}
        self.ops: list[tuple[str, str]] = []
        self.error: Exception | None = None

    def _check(self, op: str, key: str) -> None:
        self.ops.append((op, key))
        if self.error is not None:
            raise self.error

    def set_nx(self, key: str, value: str, ttl_seconds: int) -> bool:
        self._check("set_nx", key)
        if key in self.data:
            return False
        self.data[key], self.ttls[key] = value, ttl_seconds
        return True

    def set(self, key: str, value: str, ttl_seconds: int) -> None:
        self._check("set", key)
        self.data[key], self.ttls[key] = value, ttl_seconds

    def get(self, key: str) -> str | None:
        self._check("get", key)
        return self.data.get(key)

    def delete(self, key: str) -> None:
        self._check("delete", key)
        self.data.pop(key, None)

    def delete_if_value(self, key: str, value: str) -> None:
        self._check("delete_if_value", key)
        if self.data.get(key) == value:
            del self.data[key]
