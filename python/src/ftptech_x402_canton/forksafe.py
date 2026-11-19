"""Locks and per-process tables must not cross a fork.

A child inherits every lock in the state it had at fork time. If another thread of the
parent (the reconciler, a request) held one then, the child would wait for it for ever, and
it would report the parent's pending releases as its own. Each object that holds a lock or
a table registers here and resets itself in the child.
"""

from __future__ import annotations

import os
import weakref
from typing import Protocol


class ForkAware(Protocol):
    def reset_after_fork(self) -> None: ...


_objects: weakref.WeakSet[ForkAware] = weakref.WeakSet()


def register(obj: ForkAware) -> None:
    _objects.add(obj)


def _in_child() -> None:
    for obj in list(_objects):
        obj.reset_after_fork()


if hasattr(os, "register_at_fork"):
    os.register_at_fork(after_in_child=_in_child)
