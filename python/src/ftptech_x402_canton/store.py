"""The state that makes one payment produce one delivery."""

from __future__ import annotations

import json
from dataclasses import dataclass
from typing import Protocol

#: How long settled, attempt and delivered records live. The facilitator keeps
#: its own record for 24 hours; 26 hours leaves a margin.
RECORD_TTL_SECONDS = 26 * 3600

STATE_ATTEMPT = "attempt"  # /settle was about to be called, or its outcome is unknown
STATE_SETTLED = "settled"  # the facilitator reported the payment settled

#: The value of a claim whose response was written starts with this; the rest is the owner's token.
DELIVERED_PREFIX = "delivered:"


def delivered_mark(token: str) -> str:
    return DELIVERED_PREFIX + token


def is_delivered_mark(value: str) -> bool:
    return value.startswith(DELIVERED_PREFIX)


@dataclass(frozen=True)
class Record:
    """What the gate remembers about a payment, keyed by its canonical preparedTxHash."""

    state: str
    #: Binds the record to the exact signed payload and route terms the facilitator
    #: was given. A retry skips /verify only when it matches.
    digest: str
    #: The ledger update of a settled payment; empty for an attempt.
    update_id: str = ""
    #: The payer the facilitator proved, for the handler on a retry.
    payer: str = ""
    #: When an attempt was recorded (unix seconds). An attempt older than the
    #: facilitator's own record can no longer be cleared by a refusal.
    at: int = 0

    def to_json(self) -> str:
        return json.dumps(
            {
                "state": self.state,
                "digest": self.digest,
                "updateId": self.update_id,
                "payer": self.payer,
                "at": self.at,
            },
            separators=(",", ":"),
        )

    @staticmethod
    def from_json(text: str) -> Record:
        data = json.loads(text)
        if not isinstance(data, dict) or not isinstance(data.get("state"), str):
            raise ValueError("stored record is not a canton payment record")
        return Record(
            state=data["state"],
            digest=str(data.get("digest", "")),
            update_id=str(data.get("updateId", "")),
            payer=str(data.get("payer", "")),
            at=int(data.get("at") or 0),
        )


class Store(Protocol):
    """Keeps the state of paid requests. Blocking calls; safe for concurrent use.

    - a lock per canonical hash while a request works on the payment, owned by a
      per-request token;
    - a :class:`Record` per canonical hash: attempt, then settled with the updateId;
    - a claim on the updateId, owned by a token, taken before the response is
      written and marked delivered after it was: the claim is the delivery. Only a
      claim marked delivered is answered as already delivered (the final 402); an
      unmarked claim of another token answers 503.

    ``lock`` and ``claim`` must be atomic (of two concurrent calls with the same
    key exactly one returns True). ``unlock`` and ``unclaim`` must delete only
    when the stored token equals the one given (compare-and-delete). Records and
    claims must live for the given ttl at least. A failing call raises.

    A store never evicts a live lock, record or claim to make room: when it is
    full it raises ``StoreFullError`` from ``lock`` and the gate answers 503.
    """

    def lock(self, hash: str, token: str, ttl: float) -> bool: ...

    def lock_holder(self, hash: str) -> str | None: ...

    def unlock(self, hash: str, token: str) -> None: ...

    def record(self, hash: str) -> Record | None: ...

    def put_record(self, hash: str, record: Record, ttl: float) -> None: ...

    def delete_record(self, hash: str) -> None: ...

    def claim(self, update_id: str, token: str, ttl: float) -> bool: ...

    def claimant(self, update_id: str) -> str | None: ...

    def unclaim(self, update_id: str, token: str) -> None: ...

    def mark_delivered(self, update_id: str, token: str, ttl: float) -> None:
        """Rewrite the claim of ``token`` to its delivered mark (``delivered:<token>``)."""
        ...
