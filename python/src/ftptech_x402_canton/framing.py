"""Request adapters and response framing shared by the ASGI and WSGI drivers."""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any
from urllib.parse import parse_qs

from .responses import (
    HEADER_CONTENT_LENGTH,
    HEADER_TRANSFER_ENCODING,
    GateResponse,
    Headers,
)

#: How much unread request body the gate reads after a paid handler returns, so a
#: payer who disconnects during settle is seen before the claim.
DRAIN_BOUND = 256 << 10


class HeaderAdapter:
    """The SDK's ``HTTPAdapter`` over a plain header list: framework-neutral."""

    def __init__(
        self,
        method: str,
        path: str,
        url: str,
        headers: Mapping[str, str],
        query_string: str = "",
    ) -> None:
        self._method = method
        self._path = path
        self._url = url
        self._headers = headers  # keys lower-case
        self._query = query_string

    def get_header(self, name: str) -> str | None:
        return self._headers.get(name.lower())

    def get_method(self) -> str:
        return self._method

    def get_path(self) -> str:
        return self._path

    def get_url(self) -> str:
        return self._url

    def get_accept_header(self) -> str:
        return self._headers.get("accept", "")

    def get_user_agent(self) -> str:
        return self._headers.get("user-agent", "")

    def get_query_params(self) -> dict[str, str | list[str]]:
        parsed = parse_qs(self._query, keep_blank_values=True)
        return {k: v[0] if len(v) == 1 else v for k, v in parsed.items()}

    def get_query_param(self, name: str) -> str | list[str] | None:
        return self.get_query_params().get(name)

    def get_body(self) -> Any:
        return None


def framed(response: GateResponse, method: str = "GET") -> tuple[Headers, bytes]:
    """The response headers with the framing the gate owns.

    The body is buffered whole, so Content-Length is the exact length and any
    Transfer-Encoding the handler set is dropped. A 1xx, 204 or 304 carries no body,
    and neither does the answer to a HEAD (its Content-Length stays: some servers send
    the body of a write() as it is, which corrupts a keep-alive connection).
    """
    headers = [
        (k, v)
        for k, v in response.headers
        if k.lower() not in (HEADER_CONTENT_LENGTH.lower(), HEADER_TRANSFER_ENCODING.lower())
    ]
    if response.status in (204, 304) or 100 <= response.status < 200:
        return headers, b""
    headers.append((HEADER_CONTENT_LENGTH, str(len(response.body))))
    return headers, b"" if method.upper() == "HEAD" else response.body
