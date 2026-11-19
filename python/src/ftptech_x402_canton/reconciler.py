"""The background reconciler: retries what a store outage left behind in this process.

Claims and locks that could not be released, records only the in-process fallback
holds, attempts whose clear failed: each is retried every ``interval`` seconds until
it succeeds or its TTL ends. A daemon thread, started once per process, stopped by
``CantonServer.close()``, which runs one last pass and logs at error what is left.
"""

from __future__ import annotations

import logging
import os
import threading

from . import forksafe
from .ledger import Ledger
from .logs import event


def log_left_behind(ledger: Ledger, logger: logging.Logger) -> None:
    left = ledger.left_behind()
    if any(left.values()):
        event(
            logger,
            logging.ERROR,
            "background reconciler stopped with work left: these are not released by this process",
            **left,
        )


class Reconciler:
    def __init__(self, ledger: Ledger, interval: float, logger: logging.Logger) -> None:
        self._ledger = ledger
        self._interval = interval
        self._log = logger
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None
        self._pid = 0
        self._mutex = threading.Lock()
        forksafe.register(self)

    def reset_after_fork(self) -> None:
        """A thread does not survive a fork: the child starts its own with the next paid request."""
        self._mutex = threading.Lock()
        self._thread = None
        self._pid = 0

    def reopen(self) -> None:
        """Allow the thread to start again after ``stop`` (a server started a second time)."""
        self._stop = threading.Event()
        self._thread = None
        self._pid = 0

    @property
    def running(self) -> bool:
        thread = self._thread
        return thread is not None and thread.is_alive() and self._pid == os.getpid()

    def ensure_started(self) -> None:
        """Start the thread in this process (a thread does not survive a fork)."""
        if self._stop.is_set() or self.running:
            return
        with self._mutex:
            if self._stop.is_set() or self.running:
                return
            self._pid = os.getpid()
            self._thread = threading.Thread(target=self._run, name="canton-reconciler", daemon=True)
            self._thread.start()

    def _run(self) -> None:
        while not self._stop.wait(self._interval):
            self.pass_once()

    def pass_once(self, final: bool = False) -> None:
        try:
            self._ledger.reconcile_once(final)
        except Exception as error:  # a failing store must never end the thread
            event(
                self._log,
                logging.ERROR,
                "reconciler pass failed",
                errorType=type(error).__name__,
                error=str(error),
            )

    def stop(self) -> None:
        """Stop the thread, run one last pass and log what is left.

        What it stops is captured at the start: a server started again meanwhile (a second
        lifespan) has its own event and thread, which this call must not touch.
        """
        stop_event, thread = self._stop, self._thread
        stop_event.set()
        if thread is not None and thread.is_alive() and thread is not threading.current_thread():
            thread.join(self._interval + 5)
        self.pass_once(final=True)
        log_left_behind(self._ledger, self._log)
