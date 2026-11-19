"""Configuration: routes, prices and timeouts, with every problem reported at once."""

from __future__ import annotations

import logging
import math
import re
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass, field
from typing import Any
from urllib.parse import urlsplit

import httpx

from .errors import CantonConfigError
from .mechanisms.canton import CANTON_MAINNET_CAIP2, SCHEME_EXACT
from .price import Price
from .store import Store

#: FTP's public MainNet facilitator.
DEFAULT_FACILITATOR_URL = "https://facilitator.ftptech.xyz"

DEFAULT_EXECUTE_BEFORE_SECONDS = 120
MIN_EXECUTE_BEFORE_SECONDS = 1
#: The facilitator's CANTON_X402_TF_MAX_EXECUTE_BEFORE_S default.
MAX_EXECUTE_BEFORE_SECONDS = 600
MAX_MEMO_BYTES = 256
#: One /settle can take about 57 s: 45 s execute + 20 x 600 ms poll.
DEFAULT_SETTLE_TIMEOUT = 65.0
DEFAULT_SETTLE_CALLS = 4
DEFAULT_VERIFY_TIMEOUT = 30.0
DEFAULT_SUPPORTED_WAIT = 60.0
DEFAULT_SUPPORTED_ATTEMPT_TIMEOUT = 10.0
DEFAULT_RETRY_AFTER_CAP = 5.0
DEFAULT_SUPPORTED_RETRY_PAUSE = 1.0
DEFAULT_BACKOFF = (1.0, 2.0, 4.0)
#: Threads for the ASGI driver's blocking facilitator and store calls.
DEFAULT_WORKER_THREADS = 64
DEFAULT_RECONCILE_INTERVAL = 2.0
DEFAULT_BODY_DRAIN_TIMEOUT = 30.0
DEFAULT_MAX_UNREAD_BODY_BYTES = 16 << 20
#: How long one store call may take, for sizing the request budget. The store's
#: own client (for Redis: socket_timeout) is what enforces it.
STORE_CALL_BUDGET = 10.0
#: Store calls one paid request can make after /verify: read and write around
#: /settle, read, write and claim for the delivery, the delivered mark and the lock release.
POST_VERIFY_STORE_CALLS = 7


def store_call_budget(store: object) -> float:
    """The per-call store budget: ``STORE_CALL_BUDGET``, or more when the store says its own client
    can take longer (a Redis client with raised timeouts has ``call_budget_seconds``)."""
    own = getattr(store, "call_budget_seconds", 0.0)
    return max(STORE_CALL_BUDGET, float(own) if isinstance(own, (int, float)) else 0.0)


PARTY_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]*::1220[0-9a-f]{64}")
ALLOWED_METHODS = ("GET", "POST", "PUT", "PATCH", "DELETE")
# Pattern syntax of the SDK (*, :x, [x]) and URL syntax (?, #); a literal path
# has none of them. % is refused too: the SDK matches the decoded path, so an
# escaped key would never match.
FORBIDDEN_PATH_CHARS = frozenset("*:[]?#% \t\r\n\\")


@dataclass(frozen=True)
class Route:
    """The payment terms of one paid route."""

    #: The merchant party, ``<hint>::1220<64 hex>``.
    pay_to: str
    #: What one call costs: ``cc(...)``, ``token(...)`` or ``atomic(...)``.
    price: Price
    #: How long the signed payment stays valid. Default 120, allowed 1..600.
    execute_before_seconds: int | None = None
    #: Optional: at most 256 UTF-8 bytes, no leading or trailing whitespace.
    memo: str | None = None
    #: Goes into the 402 resource info.
    description: str | None = None
    #: Goes into the 402 resource info.
    mime_type: str | None = None


@dataclass
class Config:
    """Configuration of :class:`~ftptech_x402_canton.CantonServer`. Only ``routes`` is required."""

    #: ``"METHOD /literal/path"`` to the route's payment terms.
    routes: Mapping[str, Route]
    facilitator_url: str = DEFAULT_FACILITATOR_URL
    #: Keeps the one-delivery state. Default: a memory store (one process only).
    store: Store | None = None
    #: The time you allow verify + handler together; it sizes the in-flight lock.
    #: It does not cut your handler off. Default: the largest route lifetime.
    timeout: float | None = None
    #: Deadline of one /verify call.
    verify_timeout: float = DEFAULT_VERIFY_TIMEOUT
    #: Deadline of one /settle call (per connect/read phase).
    settle_timeout: float = DEFAULT_SETTLE_TIMEOUT
    #: The most /settle calls for one paid request, the SDK's own retry included.
    settle_calls: int = DEFAULT_SETTLE_CALLS
    #: Let a 3xx from a paid handler be settled. Default False: a 3xx becomes a 502 and is not settled.
    settle_on_redirect: bool = False
    #: How long startup retries GET /supported.
    supported_wait: float = DEFAULT_SUPPORTED_WAIT
    #: Deadline of one startup attempt (GET /supported).
    supported_attempt_timeout: float = DEFAULT_SUPPORTED_ATTEMPT_TIMEOUT
    #: The wait between startup attempts.
    supported_retry_pause: float = DEFAULT_SUPPORTED_RETRY_PAUSE
    #: Longest a facilitator Retry-After may hold a paid request.
    retry_after_cap: float = DEFAULT_RETRY_AFTER_CAP
    #: The wait before settle retry 1, 2, 3 when the facilitator sends no Retry-After.
    settle_backoff: Sequence[float] = DEFAULT_BACKOFF
    #: Threads for the blocking facilitator and store calls of the ASGI driver. It also
    #: caps the paid requests in flight at once on one process: more get a fast 503.
    worker_threads: int = DEFAULT_WORKER_THREADS
    #: How often the background reconciler retries what a store outage left behind.
    reconcile_interval: float = DEFAULT_RECONCILE_INTERVAL
    #: ASGI and WSGI: how long, in total, the gate waits for the rest of a request body the handler left
    #: unread, before it treats the payer as gone (nothing is settled).
    body_drain_timeout: float = DEFAULT_BODY_DRAIN_TIMEOUT
    #: ASGI and WSGI: the most unread request body bytes the gate discards; past it the payer is treated as gone.
    max_unread_body_bytes: int = DEFAULT_MAX_UNREAD_BODY_BYTES
    #: Used for facilitator calls. Default: a plain client. Every call carries its own timeout.
    http_client: httpx.Client | None = None
    logger: logging.Logger | None = None
    #: Replaced by tests; do not set.
    sleep: Callable[[float], None] | None = field(default=None, repr=False)


@dataclass(frozen=True)
class CheckedRoute:
    """A route that passed validation, with defaults applied."""

    key: str
    method: str
    path: str
    route: Route
    execute_before_seconds: int


def loose_path(path: str) -> str:
    """Case folded, dot segments and repeated slashes resolved, trailing slash dropped."""
    segments: list[str] = []
    for segment in path.split("/"):
        if segment in ("", "."):
            continue
        if segment == "..":
            if segments:
                segments.pop()
            continue
        segments.append(segment)
    return ("/" + "/".join(segments)).lower()


def loose_key(method: str, path: str) -> str:
    """HEAD is gated through the GET route by the SDK."""
    return f"{'GET' if method == 'HEAD' else method} {loose_path(path)}"


def check_routes(routes: Mapping[str, Route]) -> tuple[list[CheckedRoute], list[str]]:
    """Validate every route key and field. All problems are returned together."""
    if not routes:
        return [], ["routes is empty: configure at least one paid route"]
    checked: list[CheckedRoute] = []
    problems: list[str] = []
    seen: dict[str, str] = {}
    for key in sorted(routes):
        route_problems: list[str] = []
        route = _check_route(key, routes[key], route_problems)
        if route is None or route_problems:
            problems.extend(f"route {key!r}: {p}" for p in route_problems)
            continue
        loose = loose_key(route.method, route.path)
        if loose in seen:
            problems.append(
                f"route {key!r} duplicates route {seen[loose]!r} (paths are compared case-insensitively)"
            )
            continue
        seen[loose] = key
        checked.append(route)
    return checked, problems


def _check_route(key: str, route: Route, problems: list[str]) -> CheckedRoute | None:
    split = _split_route_key(key)
    if isinstance(split, str):
        problems.append(split)
        return None
    method, path = split
    seconds = route.execute_before_seconds
    if seconds is None:
        seconds = DEFAULT_EXECUTE_BEFORE_SECONDS
    for problem in (
        _check_pay_to(route.pay_to),
        route.price.problem()
        if isinstance(route.price, Price)
        else "price is not set; use cc(), token() or atomic()",
        _check_execute_before(seconds),
        _check_memo(route.memo),
    ):
        if problem:
            problems.append(problem)
    return CheckedRoute(key, method, path, route, seconds)


def _split_route_key(key: str) -> tuple[str, str] | str:
    method, space, path = key.partition(" ")
    if not space or any(c in path for c in " \t") or key.strip() != key:
        return 'key must be "METHOD /path" with one space'
    if method not in ALLOWED_METHODS:
        return f"method {method!r} must be one of {', '.join(ALLOWED_METHODS)}"
    problem = _check_literal_path(path)
    return problem if problem else (method, path)


def _check_literal_path(path: str) -> str | None:
    if not path.startswith("/"):
        return f"path {path!r} must start with /"
    if any(c in FORBIDDEN_PATH_CHARS for c in path):
        return f"path {path!r} must be literal: no * :x [x] ? # % or spaces"
    if any(c < " " or c > "~" for c in path):
        return f"path {path!r} must be printable ASCII"
    # The SDK strips a trailing slash from the request path but not from the key,
    # so "/paid/" would silently never match; dot segments and repeated slashes
    # never match either.
    if path != "/" and (path.endswith("/") or "//" in path or {".", ".."} & set(path.split("/"))):
        return f"path {path!r} must be canonical: no trailing slash, //, or dot segments"
    return None


def _check_pay_to(pay_to: str) -> str | None:
    if not isinstance(pay_to, str) or PARTY_RE.fullmatch(pay_to) is None:
        return f"pay_to {pay_to!r} must look like <hint>::1220<64 hex>"
    return None


def _check_execute_before(seconds: Any) -> str | None:
    if (
        isinstance(seconds, bool)
        or not isinstance(seconds, int)
        or not MIN_EXECUTE_BEFORE_SECONDS <= seconds <= MAX_EXECUTE_BEFORE_SECONDS
    ):
        return (
            f"execute_before_seconds {seconds!r} must be an integer "
            f"{MIN_EXECUTE_BEFORE_SECONDS}..{MAX_EXECUTE_BEFORE_SECONDS}"
        )
    return None


def _check_memo(memo: str | None) -> str | None:
    if memo is None or memo == "":
        return None
    if not isinstance(memo, str):
        return "memo must be a string"
    try:
        encoded = memo.encode("utf-8")
    except UnicodeEncodeError:
        return f"memo must be valid UTF-8 of at most {MAX_MEMO_BYTES} bytes"
    if len(encoded) > MAX_MEMO_BYTES:
        return f"memo must be valid UTF-8 of at most {MAX_MEMO_BYTES} bytes"
    if memo.strip() != memo:
        return "memo must have no leading or trailing whitespace"
    return None


def check_token_admins(routes: Sequence[CheckedRoute], supported_admins: set[str]) -> list[str]:
    """Refuse a token whose admin the facilitator does not list. When /supported
    lists no instruments at all, the check is skipped."""
    if not supported_admins:
        return []
    return [
        f"route {r.key!r}: token admin {r.route.price.instrument.admin!r} is not in the "
        "facilitator's /supported instruments"
        for r in routes
        if not r.route.price.is_canton_coin
        and r.route.price.instrument.admin not in supported_admins
    ]


def sdk_routes(routes: Sequence[CheckedRoute]) -> dict[str, Any]:
    """The SDK route table. Every route is authorization flow: no paymentFlow is
    set, so the scheme's default (settle after the handler) applies and upfront is
    never offered."""
    table: dict[str, Any] = {}
    for item in routes:
        price = item.route.price
        extra: dict[str, Any] = {
            "instrumentId": {"admin": price.instrument.admin, "id": price.instrument.id},
            "executeBeforeSeconds": item.execute_before_seconds,
        }
        if item.route.memo:
            extra["memo"] = item.route.memo
        entry: dict[str, Any] = {
            "accepts": {
                "scheme": SCHEME_EXACT,
                "network": CANTON_MAINNET_CAIP2,
                "payTo": item.route.pay_to,
                "maxTimeoutSeconds": item.execute_before_seconds,
                "price": {"amount": price.atomic, "asset": price.asset, "extra": extra},
            },
        }
        if item.route.description:
            entry["description"] = item.route.description
        if item.route.mime_type:
            entry["mimeType"] = item.route.mime_type
        table[item.key] = entry
    return table


@dataclass(frozen=True)
class Resolved:
    """A Config with defaults applied and every field checked."""

    config: Config
    routes: list[CheckedRoute]
    facilitator_url: str
    timeout: float
    logger: logging.Logger

    @property
    def lifetime(self) -> float:
        return float(
            max(
                (r.execute_before_seconds for r in self.routes),
                default=DEFAULT_EXECUTE_BEFORE_SECONDS,
            )
        )


def resolve(config: Config) -> Resolved:
    """Apply defaults and check everything. Raises ``CantonConfigError`` with all problems."""
    routes, problems = check_routes(config.routes)
    url = config.facilitator_url or DEFAULT_FACILITATOR_URL
    parts = urlsplit(url)
    if parts.scheme not in ("http", "https") or not parts.netloc:
        problems.append(f"facilitator_url {url!r} must be an http(s) URL")
    largest = float(
        max((r.execute_before_seconds for r in routes), default=DEFAULT_EXECUTE_BEFORE_SECONDS)
    )
    timeout = config.timeout if config.timeout is not None else largest
    for name, value in (
        ("timeout", timeout),
        ("verify_timeout", config.verify_timeout),
        ("settle_timeout", config.settle_timeout),
        ("supported_wait", config.supported_wait),
        ("supported_attempt_timeout", config.supported_attempt_timeout),
        ("supported_retry_pause", config.supported_retry_pause),
        ("retry_after_cap", config.retry_after_cap),
        ("reconcile_interval", config.reconcile_interval),
        ("body_drain_timeout", config.body_drain_timeout),
    ):
        if not _is_positive(value):
            problems.append(f"{name} {value!r} must be a positive number")
    for name, count in (
        ("settle_calls", config.settle_calls),
        ("worker_threads", config.worker_threads),
        ("max_unread_body_bytes", config.max_unread_body_bytes),
    ):
        if isinstance(count, bool) or not isinstance(count, int) or count < 1:
            problems.append(f"{name} {count!r} must be a positive integer")
    if any(not _is_non_negative(v) for v in config.settle_backoff):
        problems.append("settle_backoff must hold non-negative numbers")
    if problems:
        raise CantonConfigError(problems)
    logger = config.logger or logging.getLogger("ftptech_x402_canton")
    return Resolved(config, routes, url, timeout, logger)


def request_budget(config: Config) -> float:
    """The longest one paid request can take with this config, in seconds.

    The larger of ``timeout`` and the longest route lifetime, every /settle call,
    the pauses between them, and the store calls after /verify. Use it as the
    minimum for your proxy's read timeout, the worker timeout and the shutdown
    grace. 465 s with the defaults.
    """
    lifetime = float(
        max(
            (r.execute_before_seconds for r in config.routes.values() if r.execute_before_seconds),
            default=DEFAULT_EXECUTE_BEFORE_SECONDS,
        )
    )
    timeout = config.timeout if config.timeout is not None else lifetime
    return (
        max(timeout, lifetime)
        + config.settle_calls * config.settle_timeout
        + max(config.settle_calls - 1, 0) * config.retry_after_cap
        + POST_VERIFY_STORE_CALLS * store_call_budget(config.store)
    )


def _is_positive(value: Any) -> bool:
    return (
        isinstance(value, (int, float))
        and not isinstance(value, bool)
        and math.isfinite(value)
        and value > 0
    )


def _is_non_negative(value: Any) -> bool:
    return (
        isinstance(value, (int, float))
        and not isinstance(value, bool)
        and math.isfinite(value)
        and value >= 0
    )
