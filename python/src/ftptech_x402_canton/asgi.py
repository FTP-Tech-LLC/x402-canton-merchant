"""ASGI driver (FastAPI, Starlette): ``CantonPaymentMiddleware``.

A pure ASGI middleware. The blocking parts (facilitator calls, store calls) run on
the server's worker threads; the paid handler runs on the event loop as usual.

Payer disconnects. uvicorn and hypercorn tell the app about a lost connection
with an ``http.disconnect`` message from ``receive()``; uvicorn does not cancel
the app task, other servers may. A small pump reads ``receive()`` for the whole
paid request, so the gate always knows whether the payer is still there, and the
paid handler reads its body from the pump. Once the handler has answered, the pump
discards what is left of the request body without back-pressure (an unread body in
front of the client's FIN would hide a disconnect, and on hypercorn would block
``send()``). Everything from /verify to the end of the claim runs in a task the
server cannot cancel, so a cancelled request never cuts a settle short.

The paid handler's response is buffered whole. When its last body chunk has been
captured the gate settles and writes while the app finishes: a Starlette
``BackgroundTask`` runs alongside the settle, not before it.
"""

from __future__ import annotations

import asyncio
import logging
from collections import deque
from collections.abc import Awaitable, Callable, MutableMapping
from typing import Any, TypeVar
from urllib.parse import quote

from x402.http.types import HTTPRequestContext

from .framing import HeaderAdapter, framed
from .gate import HandlerResponse, PaidRequest, RouteDecision
from .logs import event
from .payer import PAYER_KEY, RETRY_KEY
from .responses import MSG_BUSY, MSG_NOT_FOUND, MSG_NOT_READY, GateResponse, gate_error
from .server import CantonServer

Scope = MutableMapping[str, Any]
Message = MutableMapping[str, Any]
Receive = Callable[[], Awaitable[Message]]
Send = Callable[[Message], Awaitable[None]]
ASGIApp = Callable[[Scope, Receive, Send], Awaitable[None]]

T = TypeVar("T")

#: A buffered paid response cannot use these: they send bytes without passing through us.
_UNBUFFERABLE_EXTENSIONS = (
    "http.response.pathsend",
    "http.response.zerocopysend",
    "http.response.trailers",
)
MAX_QUEUED_MESSAGES = 8


_lambda_warned = False


def _warn_lambda(logger: logging.Logger) -> None:
    global _lambda_warned
    if not _lambda_warned:
        _lambda_warned = True
        event(
            logger,
            logging.ERROR,
            "AWS Lambda (Mangum) is unsupported for paid routes: the gate never sees a payer who left, "
            "and API Gateway cuts a request at 29 to 30 s while a settle can take about 57 s. "
            "A payer who leaves during settle is charged and the retry is answered 402 payment_already_redeemed",
        )


class ReceivePump:
    """Reads the server's ``receive()`` for the whole request, with back-pressure.

    ``gone`` turns True when the server reports ``http.disconnect`` (or the request
    task is cancelled). The paid handler gets its messages from :meth:`receive`.
    """

    def __init__(self, receive: Receive) -> None:
        self._receive = receive
        self._messages: deque[Message] = deque()
        self._final: Message | None = None
        self._wakeup = asyncio.Event()
        self._space = asyncio.Event()
        self._finished = (
            asyncio.Event()
        )  # the body is complete, the payer left, or too much was discarded
        self._task: asyncio.Future[None] | None = None
        self._discarding = False
        self._discarded = 0
        self._discard_limit = 0
        self.body_complete = False
        self.gone = False
        self.drain_failed = False
        self.overflow = False

    def start(self) -> None:
        self._task = asyncio.ensure_future(self._run())

    def stop(self) -> None:
        if self._task is not None and not self._task.done():
            self._task.cancel()

    def mark_gone(self) -> None:
        self.gone = True

    def is_gone(self) -> bool:
        """For the gate: the payer left, or the rest of its body never arrived (treated the same)."""
        return self.gone or self.drain_failed

    async def _run(self) -> None:
        try:
            while True:
                while not self._discarding and len(self._messages) >= MAX_QUEUED_MESSAGES:
                    self._space.clear()
                    await self._space.wait()
                message = await self._receive()
                if message["type"] == "http.disconnect":
                    self.gone, self._final = True, message
                    self._wakeup.set()
                    self._finished.set()
                    return
                if self._discarding:
                    self._discarded += len(message.get("body", b""))
                    if self._discarded > self._discard_limit and not self.overflow:
                        # Past the cap the answer will be "payer treated as gone", but reading
                        # goes on (and drops): a server whose bounded app queue is full (hypercorn)
                        # blocks the gate's own send() until the body has been read.
                        self.overflow = True
                        self._finished.set()
                else:
                    self._messages.append(message)
                if message["type"] == "http.request" and not message.get("more_body", False):
                    self.body_complete = True
                    self._finished.set()
                self._wakeup.set()
        except asyncio.CancelledError:
            raise
        except Exception:
            self.gone = True
            self._final = {"type": "http.disconnect"}
            self._wakeup.set()
            self._finished.set()

    async def receive(self) -> Message:
        while True:
            if self._messages:
                message = self._messages.popleft()
                self._space.set()
                return message
            if self._final is not None:
                return self._final
            self._wakeup.clear()
            await self._wakeup.wait()

    def start_discard(self, max_bytes: int) -> None:
        """Read and drop the rest of the body from now on, without waiting for it: a handler that
        refused at once (413, 401, 429) is answered before the upload ends."""
        self._discarding = True
        self._discard_limit = max_bytes
        self._messages.clear()
        self._space.set()

    async def discard_rest(self, timeout: float, max_bytes: int) -> bool:
        """Read and drop what the handler left of the request body, without back-pressure.

        An unread body hides a disconnect (the server watches the connection only after
        the body is read) and, on hypercorn, blocks ``send()``. Returns False when the rest
        did not arrive within ``timeout`` or exceeded ``max_bytes``: the caller then treats
        the payer as gone and settles nothing.
        """
        self.start_discard(max_bytes)
        if self.body_complete or self.gone:
            return True
        try:
            await asyncio.wait_for(self._finished.wait(), timeout)
        except asyncio.TimeoutError:
            return False
        return not self.overflow


class CantonPaymentMiddleware:
    """ASGI middleware that charges the configured routes.

    ``app.add_middleware(CantonPaymentMiddleware, server=server)`` for FastAPI or
    Starlette, where ``server`` is a started :class:`CantonServer`.
    """

    def __init__(self, app: ASGIApp, server: CantonServer) -> None:
        self.app = app
        self.server = server
        self._tasks: set[asyncio.Future[Any]] = set()
        self._paid_in_flight = 0
        self._slot_tasks: set[asyncio.Future[Any]] = set()  # tasks that still hold a busy slot

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope["type"] == "websocket":
            await self._refuse_paid_websocket(scope, receive, send)
            return
        if scope["type"] != "http":
            await self.app(scope, receive, send)
            return
        ctx = build_context(scope)
        decision = self.server.gate.decide(ctx, alt_paths(scope))
        if decision is RouteDecision.FREE:
            await self.app(scope, receive, send)
        elif decision is RouteDecision.REFUSE:
            await write_response(send, gate_error(404, MSG_NOT_FOUND), method=ctx.method)
        elif decision is RouteDecision.NOT_READY:
            self.server.gate.note_not_ready()
            await write_response(send, gate_error(503, MSG_NOT_READY), method=ctx.method)
        else:
            await self._serve_paid(scope, receive, send, ctx)

    async def _refuse_paid_websocket(self, scope: Scope, receive: Receive, send: Send) -> None:
        ctx = build_context({**scope, "type": "http", "method": "GET"})
        if self.server.gate.decide(ctx, alt_paths(scope)) is RouteDecision.FREE:
            await self.app(scope, receive, send)
            return
        await receive()  # websocket.connect
        await send({"type": "websocket.close", "code": 1008})

    async def _serve_paid(
        self, scope: Scope, receive: Receive, send: Send, ctx: HTTPRequestContext
    ) -> None:
        has_payment = bool(ctx.adapter.get_header("payment-signature"))
        if "aws.event" in scope and has_payment:
            _warn_lambda(self.server.logger)
        if has_payment and self._paid_in_flight >= self.server.config.worker_threads:
            # Every worker thread is held by a settle: a new payment would wait, with
            # its deadline running. Nothing was done for it: say so at once.
            await write_response(send, gate_error(503, MSG_BUSY), method=ctx.method)
            return
        if has_payment:
            self._paid_in_flight += (
                1  # counted at the check, not when the task starts: a burst cannot pass it
            )
        pump = ReceivePump(receive)
        pump.start()
        task = asyncio.ensure_future(self._run_paid(scope, pump, send, ctx, has_payment))
        self._tasks.add(task)  # a strong reference: the task outlives a cancelled request
        task.add_done_callback(self._task_done)
        if has_payment:
            self._slot_tasks.add(task)
            task.add_done_callback(self._release_slot)
        try:
            await asyncio.shield(task)
        except asyncio.CancelledError:
            # The server gave up on the request (a payer who left). The task goes on:
            # a settle in flight must reach its claim; it will see ``gone`` and keep
            # the body back.
            pump.mark_gone()
            raise
        finally:
            if task.done():
                pump.stop()

    def _task_done(self, task: asyncio.Future[Any]) -> None:
        self._tasks.discard(task)
        if not task.cancelled() and task.exception() is not None:
            event(
                self.server.logger,
                logging.ERROR,
                "paid request task failed",
                errorType=type(task.exception()).__name__,
            )

    def _release_slot(self, task: asyncio.Future[Any] | None) -> None:
        """Give the busy slot back, once: at the end of the paid work (a background task the app
        goes on with uses no worker thread), or here when the task is cancelled before it starts."""
        if task is not None and task in self._slot_tasks:
            self._slot_tasks.discard(task)
            self._paid_in_flight -= 1

    async def _run_paid(
        self, scope: Scope, pump: ReceivePump, send: Send, ctx: HTTPRequestContext, counted: bool
    ) -> None:
        server, gate = self.server, self.server.gate
        inline_store = server.store_is_non_blocking
        paid: PaidRequest | None = None
        app_task: asyncio.Future[None] | None = None
        delivered = False
        try:
            # Without a payment begin makes no store call: no thread hop for the unpaid offer.
            begun = await self._call(
                gate.begin, ctx, pump.is_gone, alt_paths(scope), inline=inline_store or not counted
            )
            if isinstance(begun, GateResponse):
                await write_response(send, begun, pump=pump, method=ctx.method)
                return
            paid = begun
            # An unpaid request only builds the 402 offer: no network, no thread hop.
            answer = paid.verify() if paid.state is None else await self._call(paid.verify)
            if answer is not None:
                await self._release_before_write(paid, answer)
                await write_response(send, answer, paid, pump, self._call, ctx.method)
                return
            handler, app_task = await self._run_handler(scope, pump, paid)
            if paid.will_settle(handler):
                if not await pump.discard_rest(
                    server.config.body_drain_timeout, server.config.max_unread_body_bytes
                ):
                    pump.drain_failed = (
                        True  # the rest of the body never came: treated as a payer who left
                    )
            else:
                pump.start_discard(
                    server.config.max_unread_body_bytes
                )  # answer at once, upload goes on
            response = await self._call(paid.finish, handler)
            # The settle is done and writing the answer uses no worker thread: a payer who reads a
            # large body slowly must not hold a busy slot for the whole download.
            self._release_slot(asyncio.current_task())
            await self._release_before_write(paid, response)
            delivered = await write_response(send, response, paid, pump, self._call, ctx.method)
        finally:
            pump.stop()
            if paid is not None and paid.state is not None:
                try:
                    await self._call(paid.finish_delivery, delivered, inline=inline_store)
                except Exception as error:  # the lock expires with its TTL
                    event(
                        server.logger,
                        logging.ERROR,
                        "could not finish the paid request",
                        errorType=type(error).__name__,
                    )
            self._release_slot(asyncio.current_task())
            if app_task is not None:
                await self._await_app(app_task)

    async def _release_before_write(self, paid: PaidRequest, response: GateResponse) -> None:
        """An answer that holds no claim does not need the lock: release it first, so a
        client that resends the moment it has read the answer is not told "in progress"."""
        if not response.delivered_paid and paid.state is not None:
            await self._call(paid.close, inline=self.server.store_is_non_blocking)

    async def _await_app(self, app_task: asyncio.Future[None]) -> None:
        """Let the app finish after its response (background tasks); its errors are logged."""
        try:
            await app_task
        except asyncio.CancelledError:
            raise
        except Exception as error:
            event(
                self.server.logger,
                logging.ERROR,
                "the paid app failed after its response",
                errorType=type(error).__name__,
            )

    async def _call(self, function: Callable[..., T], *args: Any, inline: bool = False) -> T:
        if inline:
            return function(*args)
        return await asyncio.get_running_loop().run_in_executor(
            self.server.executor, function, *args
        )

    async def _run_handler(
        self, scope: Scope, pump: ReceivePump, paid: PaidRequest
    ) -> tuple[HandlerResponse, asyncio.Future[None] | None]:
        """Run the paid app and buffer its answer whole, so the gate can read the final status.

        Returns as soon as the last body chunk is captured; the second value is the app
        task, still finishing (a background task), or None when the app has returned.
        """
        inner: Scope = dict(scope)
        inner[PAYER_KEY] = paid.payer
        inner[RETRY_KEY] = paid.is_retry
        inner["extensions"] = {
            k: v
            for k, v in (scope.get("extensions") or {}).items()
            if k not in _UNBUFFERABLE_EXTENSIONS
        }
        status = 0
        headers: list[tuple[str, str]] = []
        chunks: list[bytes] = []
        complete = asyncio.Event()

        async def capture(message: Message) -> None:
            nonlocal status, headers
            kind = message["type"]
            if kind == "http.response.start" and not status:
                status = int(message["status"])
                headers = [
                    (k.decode("latin-1"), v.decode("latin-1"))
                    for k, v in message.get("headers", [])
                ]
            elif kind == "http.response.body":
                chunks.append(bytes(message.get("body", b"")))
                if not message.get("more_body", False):
                    complete.set()
            elif kind not in ("http.response.start", "http.response.informational"):
                status = (
                    -1
                )  # a message the gate cannot buffer: the answer is refused, nothing is charged
                complete.set()

        app_task: asyncio.Future[None] = asyncio.ensure_future(
            self.app(inner, pump.receive, capture)
        )
        waiter: asyncio.Future[bool] = asyncio.ensure_future(complete.wait())
        try:
            pending: set[asyncio.Future[Any]] = {app_task, waiter}
            await asyncio.wait(pending, return_when=asyncio.FIRST_COMPLETED)
        finally:
            waiter.cancel()
        if app_task.done():
            try:
                app_task.result()
            except Exception as error:
                if not paid.is_retry:
                    raise
                # The retry of a recorded payment: a bare 500 would hide the charge.
                event(
                    self.server.logger,
                    logging.ERROR,
                    "paid handler raised on the retry of a recorded payment",
                    errorType=type(error).__name__,
                )
                return HandlerResponse(-1, [], b""), None
            if not complete.is_set():
                # The app returned before its last body chunk: a cut stream must not be settled
                # and served as a complete response.
                return HandlerResponse(-1, [], b""), None
            return HandlerResponse(status, headers, b"".join(chunks)), None
        return HandlerResponse(status, headers, b"".join(chunks)), app_task


def full_path(scope: Scope) -> str:
    """The request path with its mount prefix, whether the server put the prefix into ``path`` or not."""
    root = str(scope.get("root_path", ""))
    path = str(scope.get("path", "/"))
    return path if not root or path.startswith(root) else root + path


def alt_paths(scope: Scope) -> tuple[str, ...]:
    """The full path of a request to a mounted app: a route key may be written with or without the prefix."""
    return (full_path(scope),) if scope.get("root_path") else ()


def build_context(scope: Scope) -> HTTPRequestContext:
    """The SDK's request context. The SDK matches the escaped path and the decoded one."""
    headers: dict[str, str] = {}
    for raw_name, raw_value in scope.get("headers", []):
        headers.setdefault(raw_name.decode("latin-1").lower(), raw_value.decode("latin-1"))
    full = str(scope.get("path", "/"))
    decoded = full
    root = scope.get("root_path", "")
    if root and decoded.startswith(root):
        decoded = decoded[len(root) :] or ""
    raw = scope.get("raw_path")
    # Without raw_path (Mangum) the full path stands in for the escaped one, so a key written
    # with the mount prefix is matched as well as one without it (the SDK tries both views).
    escaped = raw.decode("ascii", "replace").split("?")[0] if raw else quote(full_path(scope))
    query = scope.get("query_string", b"").decode("latin-1")
    host = headers.get("host", "")
    url = f"{scope.get('scheme', 'http')}://{host}{full_path(scope)}"
    adapter = HeaderAdapter(
        scope.get("method", "GET"), decoded, url + (f"?{query}" if query else ""), headers, query
    )
    return HTTPRequestContext(
        adapter=adapter,
        path=escaped,
        method=scope.get("method", "GET"),
        payment_header=headers.get("payment-signature"),
        decoded_path=decoded,
    )


async def write_response(
    send: Send,
    response: GateResponse,
    paid: PaidRequest | None = None,
    pump: ReceivePump | None = None,
    call: Callable[..., Awaitable[Any]] | None = None,
    method: str = "GET",
) -> bool:
    """Write a complete answer. Returns True when a paid answer whose claim this request
    holds was written. If the payer is gone or the write fails the claim is given back,
    so the retry is served once."""
    holds_claim = response.delivered_paid and paid is not None and call is not None
    if pump is not None and pump.gone:
        # Nobody is there to read it (or the server cancelled the request): writing would only
        # raise inside the server. A claim this request holds is given back.
        if holds_claim:
            assert paid is not None and call is not None
            await call(paid.release_undelivered, "payer left before the response was written")
        return False
    headers, body = framed(response, method)
    try:
        await send(
            {
                "type": "http.response.start",
                "status": response.status,
                "headers": [(k.lower().encode("latin-1"), v.encode("latin-1")) for k, v in headers],
            }
        )
        # One body message. Delivery is confirmed only up to the server's buffers: a payer who leaves
        # in the middle of a large body may be marked delivered (keep paid bodies small). Sending in
        # chunks and watching for a disconnect cannot tell a payer who left from a server that
        # reports http.disconnect for a finished response (hypercorn on Connection: close, HTTP/1.0).
        await send({"type": "http.response.body", "body": body})
    except Exception as error:
        if holds_claim:
            assert paid is not None and call is not None
            await call(paid.release_undelivered, type(error).__name__)
        if not isinstance(error, OSError):
            raise
        # A write to a closed connection is the payer leaving: nothing more to do.
        return False
    return holds_claim
