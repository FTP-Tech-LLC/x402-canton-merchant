"""Per-request state shared by the facilitator client, the gate and the drivers."""

from __future__ import annotations

import threading
from collections.abc import Callable
from contextvars import ContextVar
from dataclasses import dataclass, field, replace

from . import forksafe
from .identity import PaymentIdentity
from .store import Record


@dataclass
class RequestState:
    """What one paid request knows about its payment.

    The SDK calls the facilitator client with the payload only; the request sets
    :data:`CURRENT_STATE` around those calls, so the client works for the right
    request even when two requests for one payment overlap (one outliving its lock).
    Mutated from worker threads and the event loop: every access goes through ``mutex``.
    """

    identity: PaymentIdentity
    token: str  # random owner of this request's lock and claim
    budget: float  # the longest this paid request may take: the in-flight lock TTL
    #: True when the payer's connection is gone. Read from any thread.
    payer_gone: Callable[[], bool]
    #: The record found before /verify: a settled or attempted payment this
    #: merchant saw earlier. It lets a retry skip /verify.
    prior: Record | None = None

    mutex: threading.Lock = field(default_factory=threading.Lock, repr=False)
    payer: str = ""  # proven payer: from an isValid:true verify, or recorded for a retry
    verified: bool = False  # /verify passed, or was skipped for a recorded payment
    verify_unavailable: bool = False
    settle_calls: int = 0
    settle_unknown: bool = False  # outcome unknown after the settle budget
    last_status: int = 0  # HTTP status of the last facilitator answer, 0 when none arrived
    last_retry_after: float = 0.0
    last_payer: str = ""
    last_tx: str = ""
    # withheld: money moved but the response is not delivered (a store failure, or
    # a payer who looks gone). The gate answers 503 with the updateId.
    withheld: bool = False
    withheld_tx: str = ""
    not_charged: bool = False  # nothing was submitted to /settle
    not_charged_message: str = ""
    redeemed: bool = False  # the updateId was already delivered
    redeemed_tx: str = ""
    claimed_tx: str = ""  # updateId this request claimed, released if the write fails
    refused: bool = False  # the facilitator gave a definite refusal

    def snapshot(self) -> RequestState:
        """A copy of the outcome flags, safe to read without the lock."""
        with self.mutex:
            return replace(self, mutex=threading.Lock())


#: The state of the paid request whose SDK call is running on this thread or task.
#: The SDK calls the facilitator client with the payload only; the request sets
#: this around those calls, so the client never has to guess the request from the
#: payload. A request that outlives its in-flight lock shares nothing with the one
#: that took the lock after it: each has its own state and its own token.
CURRENT_STATE: ContextVar[RequestState | None] = ContextVar("canton_request_state", default=None)


class StateRegistry:
    """The live request states of this process, by request token.

    Only for counting and for waiting at shutdown: the facilitator client finds its
    request through :data:`CURRENT_STATE`.
    """

    def __init__(self) -> None:
        self._mutex = threading.Lock()
        self._states: dict[str, RequestState] = {}
        forksafe.register(self)

    def reset_after_fork(self) -> None:
        self._mutex = threading.Lock()
        self._states = {}  # the parent's requests do not run in the child

    def add(self, state: RequestState) -> None:
        with self._mutex:
            self._states[state.token] = state

    def remove(self, state: RequestState) -> None:
        with self._mutex:
            self._states.pop(state.token, None)

    def __len__(self) -> int:
        with self._mutex:
            return len(self._states)
