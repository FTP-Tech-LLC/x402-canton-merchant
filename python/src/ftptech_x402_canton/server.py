"""The server object: validates the config, starts up against the facilitator, owns the SDK stack."""

from __future__ import annotations

import asyncio
import logging
import os
import threading
import time
import weakref
from collections.abc import AsyncIterator, Callable
from concurrent.futures import ThreadPoolExecutor
from contextlib import asynccontextmanager
from typing import Any

from x402 import x402ResourceServerSync
from x402.http.background_init import is_fatal_startup_init_error
from x402.http.x402_http_server import x402HTTPResourceServerSync
from x402.schemas import SupportedKind, SupportedResponse

from . import forksafe
from .config import (
    Config,
    Resolved,
    check_token_admins,
    loose_key,
    request_budget,
    resolve,
    sdk_routes,
)
from .errors import CantonConfigError, CantonNotReadyError, CantonStartupError
from .facilitator import CantonFacilitator, FacilitatorSettings, FacilitatorUnavailableError
from .gate import Gate
from .ledger import Ledger
from .logs import event
from .mechanisms.canton import CANTON_MAINNET_CAIP2, SCHEME_EXACT
from .paywall import CantonPaywall
from .reconciler import Reconciler, log_left_behind
from .scheme import FtpCantonScheme, missing_kind_extra
from .state import StateRegistry
from .store_memory import MemoryStore
from .transport import Transport

SUPPORTED_VERSION = 2
MEMORY_STORE_WARNING = (
    "memory store: one process only; use RedisStore for more than one worker or instance"
)


class _Permanent(Exception):
    """A startup problem that retrying cannot fix."""


MIN_FINAL_PASS_SECONDS = 1.0  # the least a close gives the final reconciler pass


def _finish_future(future: asyncio.Future[None]) -> None:
    if not future.done():
        future.set_result(None)


class CantonServer:
    """Canton ``exact`` payments in front of paid routes, through FTP's facilitator.

    Build it, ``start()`` it (blocking; or ``await astart()``), then put it in front
    of your app: ``CantonPaymentMiddleware`` for ASGI (FastAPI, Starlette),
    ``CantonWSGIMiddleware`` for WSGI (Flask). Construction raises
    ``CantonConfigError`` for a bad config; ``start`` raises ``CantonStartupError``.
    Nothing ever exits the process.
    """

    def __init__(self, config: Config) -> None:
        self._resolved: Resolved = resolve(config)
        self.config = config
        self.logger = self._resolved.logger
        store = config.store if config.store is not None else MemoryStore()
        if isinstance(store, MemoryStore):
            self.logger.warning(MEMORY_STORE_WARNING)
        self.store = store
        self.ledger = Ledger(store, self.logger)
        self.registry = StateRegistry()
        settings = FacilitatorSettings(
            verify_timeout=config.verify_timeout,
            settle_timeout=config.settle_timeout,
            settle_calls=config.settle_calls,
            retry_after_cap=config.retry_after_cap,
            backoff=tuple(config.settle_backoff),
            sleep=config.sleep or time.sleep,
        )
        self.transport = Transport(self._resolved.facilitator_url, config.http_client)
        self.facilitator = CantonFacilitator(self.transport, self.ledger, settings, self.logger)
        self.reconciler = Reconciler(self.ledger, config.reconcile_interval, self.logger)
        # At interpreter exit a server that was never closed says what it left behind.
        self._finalizer = weakref.finalize(self, log_left_behind, self.ledger, self.logger)
        lifetimes = {
            loose_key(r.method, r.path): r.execute_before_seconds for r in self._resolved.routes
        }
        self.gate = Gate(self._resolved, self.ledger, self.registry, lifetimes)
        self.gate.on_paid_request = self.ensure_process_resources
        self._executor: ThreadPoolExecutor | None = None
        self._executor_pid = 0
        self._ready = False
        self._closed = False
        self._memory_warned = False
        self._creator_pid = os.getpid()
        self._close_mutex = threading.Lock()
        forksafe.register(self)

    def reset_after_fork(self) -> None:
        self._close_mutex = threading.Lock()
        self._executor = None  # threads do not survive a fork

    # -- lifecycle ----------------------------------------------------------

    @property
    def ready(self) -> bool:
        return self._ready

    def start(self) -> None:
        """Read GET /supported (retried for ``supported_wait``), check the facilitator's
        kind, and run the SDK's own initialization. Blocking. A closed server is reopened: a
        second lifespan (a test client per test, Mangum per invocation) starts it again."""
        self._reopen_if_closed()
        config = self._resolved.config
        self._check_store_within(config.supported_wait, config.supported_retry_pause)
        deadline = time.monotonic() + config.supported_wait
        attempt = 0
        while True:
            attempt += 1
            remaining = max(deadline - time.monotonic(), 0.001)
            try:
                self._try_start(min(config.supported_attempt_timeout, remaining))
                return
            except _Permanent as error:
                raise CantonStartupError(f"canton: {error}") from error
            except Exception as error:
                event(
                    self.logger,
                    logging.WARNING,
                    "facilitator /supported not usable yet",
                    attempt=attempt,
                    error=str(error),
                )
                if time.monotonic() + config.supported_retry_pause > deadline:
                    raise CantonStartupError(
                        f"canton: facilitator /supported unusable after {config.supported_wait}s: {error}"
                    ) from error
            time.sleep(config.supported_retry_pause)

    async def astart(self) -> None:
        """``start()`` on a worker thread, for an ASGI lifespan."""
        self._reopen_if_closed()
        await asyncio.get_running_loop().run_in_executor(self.executor, self.start)

    @asynccontextmanager
    async def lifespan(self, app: Any = None) -> AsyncIterator[None]:
        """An ASGI lifespan: ``FastAPI(lifespan=server.lifespan)``.

        Starts the server (``astart``) and closes it (``aclose``) when the app stops. An app with its
        own lifespan calls ``astart`` and ``aclose`` itself.
        """
        await self.astart()
        try:
            yield
        finally:
            await self.aclose()

    def _check_store_within(self, wait: float, pause: float) -> None:
        """Retry the store check within ``wait`` seconds, like /supported: a Redis that is not
        listening yet must not fail the start. A refused store (a cluster) fails at once."""
        deadline = time.monotonic() + wait
        while True:
            try:
                self._check_store()
                return
            except CantonConfigError:
                raise
            except CantonStartupError as error:
                if time.monotonic() + pause > deadline:
                    raise CantonStartupError(
                        f"canton: payment store unusable after {wait:g}s: {error}"
                    ) from error
            time.sleep(pause)

    def _check_store(self) -> None:
        """Do one round of real I/O on a store that has a ``check()`` (Redis), so a store that
        cannot work fails at startup, not at the first paid request."""
        check = getattr(self.store, "check", None)
        if check is None:
            return
        try:
            check()
        except CantonConfigError:
            raise
        except Exception as error:
            raise CantonStartupError(f"{type(error).__name__}: {error}") from error

    def _reopen_if_closed(self) -> None:
        with self._close_mutex:
            if not self._closed:
                return
            self._closed = False
            self._ready = False
            self._executor = None
            self.gate.reopen()
            self.reconciler.reopen()
            self._finalizer = weakref.finalize(self, log_left_behind, self.ledger, self.logger)

    def close(self, timeout: float | None = None) -> None:
        """Shut down: refuse new payments, wait up to ``timeout`` seconds (default: ``request_budget(config)``) for paid requests in
        flight, stop the reconciler (one last pass, what is left is logged at error) and close the pool.

        Call it after the web server has stopped accepting requests (in ASGI, in the lifespan
        shutdown; in WSGI, at exit). Once it has begun, a request that carries a payment is
        answered 503 "payment in progress: retry the same payment" (the unpaid offer is still
        answered), so nothing starts a paid flow that no reconciler will heal. Every caller waits
        up to its own ``timeout``. Requests still running when it finishes lose their delivery
        marks and lock releases, which the lock's TTL covers: it is logged at warning level.
        """
        self.gate.begin_closing()
        deadline = time.monotonic() + (
            timeout if timeout is not None else request_budget(self.config)
        )
        while len(self.registry) and time.monotonic() < deadline:
            time.sleep(0.05)
        final_pass = self._start_final_pass()
        final_pass.join(max(deadline - time.monotonic(), MIN_FINAL_PASS_SECONDS))
        if final_pass.is_alive():
            self._log_final_pass_cut()

    def _start_final_pass(self, on_end: Callable[[], None] | None = None) -> threading.Thread:
        """Run the final pass on a daemon thread: the process exit never waits for it."""

        def run() -> None:
            try:
                self._finish_close()
            finally:
                if on_end is not None:
                    on_end()

        thread = threading.Thread(target=run, name="canton-final-pass", daemon=True)
        thread.start()
        return thread

    def _log_final_pass_cut(self) -> None:
        event(
            self.logger,
            logging.ERROR,
            "the final reconciler pass did not finish within the close timeout: what it left is not released by this process",
        )

    def _finish_close(self) -> None:
        with self._close_mutex:
            if self._closed:
                return
            self._closed = True
        in_flight = len(self.registry)
        if in_flight:
            event(
                self.logger,
                logging.WARNING,
                "closed with paid requests in flight: their post-response work may be lost; "
                "close after the web server has drained",
                inFlight=in_flight,
            )
        # This run's own resources first; the slow final pass runs last, so a pass that outlives
        # aclose() never touches what a restart created after it.
        self._finalizer.detach()
        self.transport.close()
        if self._executor is not None:
            self._executor.shutdown(wait=False)
        self._executor = None
        self.reconciler.stop()

    async def aclose(self, timeout: float | None = None) -> None:
        """``close()`` for an ASGI lifespan: waits for paid requests in flight without blocking the loop."""
        self.gate.begin_closing()
        deadline = time.monotonic() + (
            timeout if timeout is not None else request_budget(self.config)
        )
        while len(self.registry) and time.monotonic() < deadline:
            await asyncio.sleep(0.05)
        # The last reconciler pass makes blocking store calls: off the event loop, on a daemon thread
        # (asyncio.run waits for its default executor at exit), and bounded by what is left of the
        # timeout, with a floor of MIN_FINAL_PASS_SECONDS.
        loop = asyncio.get_running_loop()
        finished: asyncio.Future[None] = loop.create_future()

        def signal_end() -> None:
            try:
                loop.call_soon_threadsafe(_finish_future, finished)
            except RuntimeError:
                pass  # the loop is already closed: the process is exiting

        self._start_final_pass(signal_end)
        left = max(deadline - time.monotonic(), MIN_FINAL_PASS_SECONDS)
        try:
            await asyncio.wait_for(asyncio.shield(finished), left)
        except asyncio.TimeoutError:
            self._log_final_pass_cut()

    def __enter__(self) -> CantonServer:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    @property
    def executor(self) -> ThreadPoolExecutor:
        """The threads for blocking facilitator and store calls of the ASGI driver (one pool per process)."""
        if self._closed:
            raise CantonNotReadyError("CantonServer is closed")
        pid = os.getpid()
        if self._executor is None or self._executor_pid != pid:
            self._executor = ThreadPoolExecutor(
                max_workers=self._resolved.config.worker_threads, thread_name_prefix="canton"
            )
            self._executor_pid = pid
        return self._executor

    def ensure_process_resources(self) -> None:
        """Start, in this process, what does not survive a fork: the reconciler thread."""
        if not self._closed:
            self.reconciler.ensure_started()
            self._warn_if_memory_store_in_a_worker()

    def _warn_if_memory_store_in_a_worker(self) -> None:
        """The memory store is per process: several workers deliver one replay once each, and a
        recycled worker forgets its deliveries."""
        if self._memory_warned or not isinstance(self.store, MemoryStore):
            return
        workers = os.environ.get("WEB_CONCURRENCY", "")
        forked = os.getpid() != self._creator_pid
        if forked or (workers.isdigit() and int(workers) > 1):
            self._memory_warned = True
            event(
                self.logger,
                logging.ERROR,
                "memory store with more than one worker (a forked worker, or WEB_CONCURRENCY above 1): "
                "a replay inside the payment deadline can be delivered once per worker. Use RedisStore, "
                "or run exactly one worker without max-requests recycling",
                pid=os.getpid(),
            )

    @property
    def store_is_non_blocking(self) -> bool:
        return bool(getattr(self.store, "non_blocking", False))

    def require_ready(self) -> None:
        if not self._ready:
            raise CantonNotReadyError("CantonServer.start() has not finished")

    # -- startup ------------------------------------------------------------

    def _try_start(self, attempt_timeout: float) -> None:
        supported = self.facilitator.fetch_supported(attempt_timeout)
        kind = find_kind(supported)
        problems = check_token_admins(self._resolved.routes, supported_admins(kind))
        if problems:
            raise _Permanent("; ".join(problems))
        self.facilitator.prime(supported)  # the SDK's initialization must not read /supported again
        server = x402ResourceServerSync(self.facilitator)
        server.register(CANTON_MAINNET_CAIP2, FtpCantonScheme())
        http_server = x402HTTPResourceServerSync(server, sdk_routes(self._resolved.routes))
        http_server.register_paywall_provider(CantonPaywall())
        try:
            # The SDK's adapters call this and os._exit(1) on a fatal error. We call it
            # ourselves, inside the retry, so the process is never ended.
            http_server.initialize()
        except Exception as error:
            if is_fatal_startup_init_error(error):
                raise _Permanent(f"x402 initialization failed: {error}") from error
            raise
        self.gate.http_server = http_server
        self._ready = True


def find_kind(supported: SupportedResponse) -> SupportedKind:
    """The ``{x402Version 2, exact, canton:mainnet}`` kind, with ``feePayer`` and ``synchronizerId``."""
    for kind in supported.kinds:
        if (
            kind.x402_version == SUPPORTED_VERSION
            and kind.scheme == SCHEME_EXACT
            and kind.network == CANTON_MAINNET_CAIP2
        ):
            problem = missing_kind_extra(kind)
            if problem:
                raise FacilitatorUnavailableError(problem)
            return kind
    raise FacilitatorUnavailableError(
        "facilitator /supported has no x402Version 2 exact canton:mainnet kind"
    )


def supported_admins(kind: SupportedKind) -> set[str]:
    """The token admins listed in the kind's ``extra.instruments``."""
    admins: set[str] = set()
    listed = (kind.extra or {}).get("instruments")
    for item in listed if isinstance(listed, list) else []:
        if not isinstance(item, dict):
            continue
        if isinstance(item.get("admin"), str):
            admins.add(item["admin"])
        instrument = item.get("instrumentId")
        if isinstance(instrument, dict) and isinstance(instrument.get("admin"), str):
            admins.add(instrument["admin"])
    return admins


def canton_server_sync(config: Config) -> CantonServer:
    """Build and start a server (blocking). For Flask, or any module-level setup."""
    server = CantonServer(config)
    server.start()
    return server


async def canton_server(config: Config) -> CantonServer:
    """Build and start a server without blocking the event loop (call it where the middleware is
    already in place, for example in a test; in a FastAPI app build ``CantonServer`` at import and
    ``await server.astart()`` in the lifespan)."""
    server = CantonServer(config)
    await server.astart()
    return server
