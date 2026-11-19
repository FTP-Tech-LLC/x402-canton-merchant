"""WSGI driver (Flask, or any WSGI app): ``CantonWSGIMiddleware``.

A paid request holds its worker for verify + handler + settle, up to
``request_budget(config)`` seconds: set the worker timeout to at least that
(gunicorn ``--timeout 470`` for the defaults) and raise the header size limit
(``--limit-request-field_size 32768``): a PAYMENT-SIGNATURE is about 11 KiB.

Payer disconnects. WSGI has no standard disconnect signal, so the gate uses what
the server offers, before settle and before the claim:

- waitress: ``environ["waitress.client_disconnected"]``. Start waitress with
  ``channel_request_lookahead=1`` (the default 0 turns the detection off).
- gunicorn and werkzeug: a peek at the client socket (``gunicorn.socket``,
  ``werkzeug.socket``): an EOF means the payer is gone.
- uWSGI: ``uwsgi.is_connected(uwsgi.connection_fd())``.

Where none of these is available (TLS ended in the WSGI server itself, other
servers) the gate learns only from a failed write. A payer who leaves during
settle is then not seen: the claim is taken and the retry is answered 402
``payment_already_redeemed``. End TLS in a proxy in front of the WSGI server. A warning is
logged once when no signal is available.
"""

from __future__ import annotations

import importlib
import logging
import os
import select
import socket
import ssl
import struct
import threading
import time
from collections.abc import Callable, Iterable
from http import HTTPStatus
from typing import Any
from urllib.parse import quote

from x402.http.types import HTTPRequestContext

from .config import DEFAULT_BODY_DRAIN_TIMEOUT
from .framing import DRAIN_BOUND, HeaderAdapter, framed
from .gate import HandlerResponse, PaidRequest, RouteDecision
from .payer import PAYER_KEY, RETRY_KEY
from .responses import MSG_NOT_FOUND, MSG_NOT_READY, GateResponse, gate_error
from .server import CantonServer

Environ = dict[str, Any]
StartResponse = Callable[..., Callable[[bytes], object] | None]  # None: a server without write()

#: Environ keys under which common servers expose the client socket.
SOCKET_KEYS = ("gunicorn.socket", "werkzeug.socket")
WAITRESS_KEY = "waitress.client_disconnected"
CONSUMED_KEY = "ftptech_x402_canton.input"
_log = logging.getLogger("ftptech_x402_canton")
UNDRAINED_KEY = "ftptech_x402_canton.undrained"


class CountingInput:
    """Wraps ``wsgi.input`` and counts what the app reads.

    The drain after the handler must read only what is left of the declared body: a
    read past it would wait for bytes that never come (PEP 3333 forbids it, and werkzeug's
    and wsgiref's ``wsgi.input`` is the raw socket file).
    """

    def __init__(self, stream: Any) -> None:
        self._stream = stream
        self.consumed = 0
        # Offer only what the wrapped input offers: werkzeug reads with readinto whenever the stream
        # has it, and gunicorn's and uWSGI's inputs do not (the app then falls back to read()).
        if hasattr(stream, "readinto"):
            vars(self)["readinto"] = self._readinto
        if hasattr(stream, "read1"):
            vars(self)["read1"] = self._read1

    def read(self, size: int = -1) -> bytes:
        data: bytes = self._stream.read() if size is None or size < 0 else self._stream.read(size)
        self.consumed += len(data)
        return data

    def readline(self, size: int = -1) -> bytes:
        data: bytes = (
            self._stream.readline() if size is None or size < 0 else self._stream.readline(size)
        )
        self.consumed += len(data)
        return data

    def readlines(self, hint: int = -1) -> list[bytes]:
        lines: list[bytes] = list(
            self._stream.readlines() if hint is None or hint < 0 else self._stream.readlines(hint)
        )
        self.consumed += sum(len(line) for line in lines)
        return lines

    def _readinto(self, buffer: Any) -> int:
        count: int = self._stream.readinto(buffer)
        self.consumed += count or 0
        return count

    def _read1(self, size: int = -1) -> bytes:
        data: bytes = self._stream.read1(size)
        self.consumed += len(data)
        return data

    def __iter__(self) -> Any:
        while True:
            line = self.readline()
            if not line:
                return
            yield line

    def __getattr__(self, name: str) -> Any:
        return getattr(self._stream, name)


class CantonWSGIMiddleware:
    """WSGI middleware that charges the configured routes.

    ``app.wsgi_app = CantonWSGIMiddleware(app.wsgi_app, server)`` for Flask, where
    ``server`` is a started :class:`CantonServer`.
    """

    def __init__(self, app: Callable[..., Iterable[bytes]], server: CantonServer) -> None:
        self.app = app
        self.server = server

    def __call__(self, environ: Environ, start_response: StartResponse) -> Iterable[bytes]:
        ctx = build_context(environ)
        decision = self.server.gate.decide(ctx, alt_paths(environ))
        method = ctx.method
        if decision is RouteDecision.FREE:
            return self.app(environ, start_response)
        if decision is RouteDecision.REFUSE:
            return write_response(start_response, gate_error(404, MSG_NOT_FOUND), method=method)[0]
        if decision is RouteDecision.NOT_READY:
            self.server.gate.note_not_ready()
            return write_response(start_response, gate_error(503, MSG_NOT_READY), method=method)[0]
        return self._serve_paid(environ, start_response, ctx)

    def _serve_paid(
        self, environ: Environ, start_response: StartResponse, ctx: HTTPRequestContext
    ) -> Iterable[bytes]:
        gate = self.server.gate
        begun = gate.begin(
            ctx,
            lambda: bool(environ.get(UNDRAINED_KEY)) or client_gone(environ),
            alt_paths(environ),
        )
        if isinstance(begun, GateResponse):
            return write_response(start_response, begun, method=ctx.method)[0]
        paid = begun
        delivered = False
        try:
            answer = paid.verify()
            if answer is not None:
                paid.close()  # an answer without a claim does not need the lock
                return write_response(start_response, answer, paid, environ, ctx.method)[0]
            handler = self._run_handler(environ, paid)
            config = self.server.config
            if paid.will_settle(handler) and not drain_input(
                environ, config.max_unread_body_bytes, config.body_drain_timeout
            ):
                environ[UNDRAINED_KEY] = (
                    True  # the rest of the body never came: treated as a payer who left
                )
            response = paid.finish(handler)
            if not response.delivered_paid:
                paid.close()
            body, delivered = write_response(start_response, response, paid, environ, ctx.method)
            if environ.get(UNDRAINED_KEY) and not body:
                _end_connection(environ)
            return body
        finally:
            paid.finish_delivery(delivered)

    def _run_handler(self, environ: Environ, paid: PaidRequest) -> HandlerResponse:
        """Run the paid app and buffer its answer whole, so the gate can read the final status."""
        environ[PAYER_KEY] = paid.payer
        environ[RETRY_KEY] = paid.is_retry
        stream = environ.get("wsgi.input")
        if stream is not None:
            counter = CountingInput(stream)
            environ["wsgi.input"] = counter
            environ[CONSUMED_KEY] = counter
        status = 0
        headers: list[tuple[str, str]] = []
        chunks: list[bytes] = []

        def capture(
            status_line: str, response_headers: list[tuple[str, str]], exc_info: Any = None
        ) -> Callable[[bytes], None]:
            nonlocal status, headers
            try:
                status = int(status_line.split(None, 1)[0])
            except (ValueError, IndexError):
                status = -1
            headers = list(response_headers)
            return chunks.append

        try:
            result = self.app(environ, capture)
            try:
                chunks.extend(chunk for chunk in result if chunk)
            finally:
                close = getattr(result, "close", None)
                if close is not None:
                    close()
        except Exception as error:
            if not paid.is_retry:
                raise
            # The retry of a recorded payment: a bare 500 would hide the charge.
            _log.error(
                "paid handler raised on the retry of a recorded payment: %s", type(error).__name__
            )
            return HandlerResponse(-1, [], b"")
        return HandlerResponse(status, headers, b"".join(chunks))


def full_path(environ: Environ) -> str:
    return str(environ.get("SCRIPT_NAME", "")) + (str(environ.get("PATH_INFO", "/")) or "/")


def alt_paths(environ: Environ) -> tuple[str, ...]:
    """The path with its mount prefix (SCRIPT_NAME): a route key may be written with or without it."""
    return (full_path(environ),) if environ.get("SCRIPT_NAME") else ()


def build_context(environ: Environ) -> HTTPRequestContext:
    """The SDK's request context. The SDK matches the escaped path and the decoded one."""
    headers: dict[str, str] = {}
    for key, value in environ.items():
        if key.startswith("HTTP_"):
            headers[key[5:].replace("_", "-").lower()] = str(value)
    if environ.get("CONTENT_TYPE"):
        headers["content-type"] = str(environ["CONTENT_TYPE"])
    decoded = str(environ.get("PATH_INFO", "/")) or "/"
    raw = environ.get("RAW_URI") or environ.get("REQUEST_URI")
    # Without a raw URI the full path (with SCRIPT_NAME) stands in for the escaped one.
    escaped = str(raw).split("?")[0] if raw else quote(full_path(environ))
    method = str(environ.get("REQUEST_METHOD", "GET"))
    query = str(environ.get("QUERY_STRING", ""))
    host = headers.get("host") or str(environ.get("SERVER_NAME", ""))
    url = f"{environ.get('wsgi.url_scheme', 'http')}://{host}{quote(full_path(environ))}"
    adapter = HeaderAdapter(method, decoded, url + (f"?{query}" if query else ""), headers, query)
    return HTTPRequestContext(
        adapter=adapter,
        path=escaped,
        method=method,
        payment_header=headers.get("payment-signature"),
        decoded_path=decoded,
    )


_warned: set[str] = set()


def _warn_once(kind: str, message: str) -> None:
    if kind not in _warned:
        _warned.add(kind)
        _log.error(message)


_NO_SIGNAL = (
    "a payer who leaves during settle is charged and the retry is answered 402 "
    "payment_already_redeemed: only a failed write is seen"
)


def _waitress_lookahead(check: Any) -> int | None:
    try:
        return int(check.__self__.adj.channel_request_lookahead)
    except (AttributeError, TypeError, ValueError):
        return None


def client_gone(environ: Environ) -> bool:
    """True when the server says the client is gone. False when it cannot tell (with a loud,
    one-time log naming the consequence)."""
    waitress_check = environ.get(WAITRESS_KEY)
    if callable(waitress_check):
        if _waitress_lookahead(waitress_check) == 0:
            # The callable exists but waitress does not read the socket while a task runs: it
            # can never say "gone". Trusting it would hide the problem.
            _warn_once(
                "waitress",
                "waitress runs with channel_request_lookahead=0 (its default), so the gate cannot "
                "see a payer who left: set channel_request_lookahead=1. Until then " + _NO_SIGNAL,
            )
            return False
        return bool(waitress_check())
    for key in SOCKET_KEYS:
        sock = environ.get(key)
        if isinstance(sock, socket.socket):
            if isinstance(sock, ssl.SSLSocket):
                _warn_once(
                    "tls",
                    "TLS ends in the WSGI server itself, so the gate cannot see a payer who left: "
                    "end TLS in a proxy in front of it. Until then " + _NO_SIGNAL,
                )
                return False
            return socket_gone(sock)
    if "uwsgi.version" in environ:
        return _uwsgi_gone()
    _warn_once(
        "none",
        "this WSGI server gives the gate no way to see a payer who left (supported: gunicorn, "
        "werkzeug, waitress with channel_request_lookahead=1, uWSGI). " + _NO_SIGNAL,
    )
    return False


def _uwsgi_gone() -> bool:
    """``uwsgi.is_connected`` reads only the socket's error state (an RST), never a FIN, so the
    descriptor is peeked as well, on a duplicate (the original stays uWSGI's)."""
    try:
        uwsgi = importlib.import_module("uwsgi")
        fd = int(uwsgi.connection_fd())
        if not uwsgi.is_connected(fd):
            return True
        duplicate = socket.socket(fileno=os.dup(fd))
        try:
            return socket_gone(duplicate)
        finally:
            duplicate.close()
    except Exception:
        return False


_POLLRDHUP = getattr(select, "POLLRDHUP", 0)
_POLLIN = getattr(select, "POLLIN", 1)
_POLLHUP = getattr(select, "POLLHUP", 0)
_POLLERR = getattr(select, "POLLERR", 0)
_MSG_DONTWAIT = getattr(socket, "MSG_DONTWAIT", 0)  # no MSG_DONTWAIT on Windows
_MSG_PEEK_NOWAIT = socket.MSG_PEEK | _MSG_DONTWAIT


def _poll(sock: socket.socket, timeout: float = 0.0) -> int | None:
    """The poll events of the socket now, or within ``timeout`` seconds (0: none); None when
    that cannot be checked.

    ``poll`` and ``select`` take no file descriptor of their own: a process at its fd
    limit does not turn every payer into a departed one.
    """
    try:
        if hasattr(select, "poll"):
            poller = select.poll()
            poller.register(sock.fileno(), _POLLIN | _POLLHUP | _POLLERR | _POLLRDHUP)
            events = poller.poll(int(max(timeout, 0.0) * 1000))
            return int(events[0][1]) if events else 0
        return _POLLIN if select.select([sock], [], [], max(timeout, 0.0))[0] else 0
    except (OSError, ValueError):
        return None


def _readable(sock: socket.socket) -> bool | None:
    events = _poll(sock)
    return None if events is None else bool(events)


def socket_gone(sock: socket.socket) -> bool:
    events = _poll(sock)
    if not events:
        return False  # nothing to read, or unknown: not gone
    if events & (_POLLHUP | _POLLERR | _POLLRDHUP):
        # The peer's FIN is seen even behind unread request body bytes, which a peek is not.
        return True
    try:
        # A 1-byte peek on a socket reported readable does not block, with or without MSG_DONTWAIT.
        return sock.recv(1, _MSG_PEEK_NOWAIT) == b""
    except (BlockingIOError, InterruptedError):
        return False
    except ValueError:
        return False  # a TLS socket does not take flags: unknown
    except OSError:
        return True


def _socket_of(environ: Environ) -> socket.socket | None:
    for key in SOCKET_KEYS:
        sock = environ.get(key)
        if isinstance(sock, socket.socket) and not isinstance(sock, ssl.SSLSocket):
            return sock
    return None


def _available(sock: socket.socket) -> int | None:
    """Bytes in the kernel's receive buffer (FIONREAD); None when unknown."""
    try:
        import fcntl
        import termios

        packed = fcntl.ioctl(sock.fileno(), termios.FIONREAD, b"\0\0\0\0")
        return int(struct.unpack("i", packed)[0])
    except (ImportError, OSError, ValueError):
        return None


def _uwsgi_post_buffering() -> bool:
    try:
        return bool(importlib.import_module("uwsgi").opt.get("post-buffering"))
    except Exception:
        return False


def _uwsgi_may_hold(remaining: int) -> bool:
    """Whether uWSGI may hold the ``remaining`` unread body bytes in its own buffer already."""
    try:
        uwsgi = importlib.import_module("uwsgi")
        options = uwsgi.opt
        if options.get("post-buffering"):
            return True
        return remaining <= int(options.get("buffer-size") or 4096)
    except Exception:
        return False


def _uwsgi_poll_socket(environ: Environ) -> socket.socket | None:
    """A duplicate of uWSGI's connection descriptor, to poll (never to set a timeout on: that would
    change the original). uWSGI's own read gives up after its socket timeout (4 s by default)."""
    if "uwsgi.version" not in environ:
        return None
    try:
        uwsgi = importlib.import_module("uwsgi")
        return socket.socket(fileno=os.dup(int(uwsgi.connection_fd())))
    except Exception:
        return None


def _end_connection(environ: Environ) -> None:
    """The drain gave up, so where the next request starts on this connection is unknown (a server
    counts the short read as the whole body, and what the payer sends next would run as a new
    request). The answer is already written: end the connection in both directions."""
    sock = _socket_of(environ)
    if sock is not None:
        try:
            sock.shutdown(socket.SHUT_RDWR)
            # Bytes that arrived between the drain's deadline (read side shut by the timer) and this
            # shutdown are still queued, and the server would parse them as a request: drop them.
            # Later bytes are refused by the kernel, never queued.
            while sock.recv(65536, _MSG_DONTWAIT):
                pass
        except OSError:
            pass


def _shutdown_read(sock: socket.socket) -> None:
    try:
        sock.shutdown(socket.SHUT_RD)
    except OSError:
        pass


def _drain_uwsgi_chunked(limit: int, deadline: float) -> bool:
    """uWSGI gives a WSGI app nothing of a chunked body through ``wsgi.input``: read it with ``chunked_read``."""
    try:
        uwsgi = importlib.import_module("uwsgi")
        read = 0
        while read < limit:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return False
            chunk = uwsgi.chunked_read(max(int(remaining), 1))
            if not chunk:
                return True
            read += len(chunk)
        return False
    except Exception:
        return False


def drain_input(
    environ: Environ, limit: int = DRAIN_BOUND, timeout: float = DEFAULT_BODY_DRAIN_TIMEOUT
) -> bool:
    """Read (and drop) what the handler left of the request body, so an EOF behind it is seen.

    Only the declared bytes that the handler has not read (it reads through
    :class:`CountingInput`), never past the declared length, up to ``limit`` bytes and
    ``timeout`` seconds. A body still arriving is waited for (the client socket is polled with
    the time left): the rest would otherwise fill the receive window during settle with the
    payer's FIN queued behind it. A chunked body (no CONTENT_LENGTH) is read to its end on
    servers that terminate it (``wsgi.input_terminated``) and through ``uwsgi.chunked_read``
    on uWSGI. Returns False when the bound or the time ran out, or the stream ended or failed
    before the declared end (the payer left): the caller then treats the payer as gone.
    """
    stream = environ.get("wsgi.input")
    if stream is None:
        return True
    deadline = time.monotonic() + timeout
    consumed = (
        environ[CONSUMED_KEY].consumed
        if isinstance(environ.get(CONSUMED_KEY), CountingInput)
        else 0
    )
    declared = environ.get("CONTENT_LENGTH")
    try:
        if declared in (None, ""):
            if environ.get("wsgi.input_terminated"):
                remaining = limit + 1
            elif (
                "uwsgi.version" in environ
                and "chunked" in str(environ.get("HTTP_TRANSFER_ENCODING", "")).lower()
            ):
                return _drain_uwsgi_chunked(limit, deadline)
            else:
                return True
        else:
            remaining = max(int(declared) - consumed, 0)
    except ValueError:
        return True
    want = min(remaining, limit)
    sock = _socket_of(environ)
    poll_sock = _uwsgi_poll_socket(environ) if sock is None else None
    previous = sock.gettimeout() if sock is not None else None
    # The deadline is for the whole drain, not per read: a payer who trickles bytes just under the
    # per-read timeout must not hold the worker. At the deadline the read side is shut, which ends
    # a blocked read (EOF) and counts as a payer who left.
    timer = None
    if sock is not None:
        timer = threading.Timer(timeout, _shutdown_read, (sock,))
        timer.daemon = True
        timer.start()
    read = 0
    try:
        while read < want:
            left = deadline - time.monotonic()
            if left <= 0:
                return False
            size = min(65536, want - read)
            if sock is not None:
                # A raw read gives up when the time is up instead of waiting for a body that does
                # not come (the stream may hold part of it in its own buffer, which a poll cannot see).
                sock.settimeout(left)
            # Never ask for more than is left of the declared body: a read past it would wait for
            # bytes that never come.
            if poll_sock is not None:
                # uWSGI keeps what came with the request head (up to buffer-size) or the whole body
                # (post-buffering) in its own buffer, which a poll of the socket cannot see, and a
                # read that waits past its socket timeout (4 s) fails for good. So: read at once
                # what it can hold (the remainder fits its buffer, or post-buffering is on); else
                # wait on the duplicate descriptor and read what the kernel has.
                if not _uwsgi_may_hold(want - read):
                    if not _poll(poll_sock, left):
                        return False  # the rest did not arrive in time
                    available = _available(poll_sock)
                    if available:
                        size = min(size, available)
                    elif available == 0:
                        return False  # the payer closed
                else:
                    # Never ask uWSGI for more than it can return at once: its read waits for every
                    # byte asked, each wait up to its socket timeout, which would let a trickle hold
                    # the worker past body_drain_timeout. What the kernel has, or one byte (at once
                    # from uWSGI's buffer, else within its socket timeout); the whole rest only
                    # under post-buffering, when uWSGI holds the whole body.
                    available = _available(poll_sock)
                    if _uwsgi_post_buffering():
                        size = min(size, want - read)
                    else:
                        size = min(size, available) if available else 1
            chunk = stream.read(size)
            if not chunk:
                return (
                    not declared
                )  # EOF: a terminated chunked body ended; before the declared end the payer left
            read += len(chunk)
    except Exception as error:  # a timed-out or failed read: the connection is gone or too slow
        _log.debug("drain stopped: %s", type(error).__name__)
        return False
    finally:
        if timer is not None:
            timer.cancel()
        if poll_sock is not None:
            poll_sock.close()
        if sock is not None:
            try:
                sock.settimeout(previous)
            except OSError:
                pass
    return read >= remaining  # False: the bound was reached with body left


def _reason_phrase(status: int) -> str:
    try:
        return HTTPStatus(status).phrase
    except ValueError:
        return "Status"


def write_response(
    start_response: StartResponse,
    response: GateResponse,
    paid: PaidRequest | None = None,
    environ: Environ | None = None,
    method: str = "GET",
) -> tuple[list[bytes], bool]:
    """Write a complete answer through ``start_response``'s ``write`` callable, so a
    failed write is seen here: a paid answer whose claim this request holds is then
    given back, and the retry is served once. Returns the (empty) body and whether a
    claimed answer was written."""
    holds_claim = response.delivered_paid and paid is not None
    if holds_claim and environ is not None and client_gone(environ):
        assert paid is not None
        paid.release_undelivered("payer left before the response was written")
        return [], False
    headers, body = framed(response, method)
    status_line = f"{response.status} {_reason_phrase(response.status)}"
    try:
        write = start_response(status_line, headers)
        if not callable(write):
            # start_response gave no write(): Starlette's WSGIMiddleware, asgiref's WsgiToAsgi,
            # uvicorn --interface wsgi. The body goes out as the return value (PEP 3333), and a
            # failed write cannot be seen: the delivery is treated as made.
            return ([body] if body else []), holds_claim
        if body:
            write(body)
    except Exception as error:
        if holds_claim:
            assert paid is not None
            paid.release_undelivered(type(error).__name__)
        if not isinstance(error, OSError):
            raise
        return [], False
    return [], holds_claim
