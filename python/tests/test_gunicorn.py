"""WSGI on gunicorn sync workers: the deployment notes of the README, on a real server."""

from __future__ import annotations

import os
import shutil
import socket
import subprocess
import sys
import time
from collections.abc import Iterator
from typing import Any

import httpx
import pytest

from tests.mock_facilitator import Answer, MockFacilitator
from tests.support import payment_header, tx_hash

pytestmark = pytest.mark.skipif(
    sys.platform == "win32"
    or shutil.which("gunicorn") is None
    and not os.path.exists(os.path.join(os.path.dirname(sys.executable), "gunicorn")),
    reason="gunicorn needed",
)


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def start_gunicorn(
    facilitator: MockFacilitator, *extra: str
) -> Iterator[tuple[int, MockFacilitator]]:
    port = free_port()
    env = {**os.environ, "FACILITATOR_URL": facilitator.url, "PYTHONPATH": os.getcwd()}
    process = subprocess.Popen(
        [
            sys.executable, "-m", "gunicorn", "tests.gunicorn_app:app", "--bind", f"127.0.0.1:{port}",
            "--timeout", "120", "--limit-request-field_size", "32768", "--log-level", "warning", *extra,
        ],
        env=env,
    )  # fmt: skip
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.2):
                break
        except OSError:
            time.sleep(0.1)
    else:
        process.kill()
        pytest.fail("gunicorn did not start")
    yield port, facilitator
    process.terminate()
    process.wait(10)


@pytest.fixture
def gunicorn(facilitator: MockFacilitator) -> Iterator[tuple[int, MockFacilitator]]:
    yield from start_gunicorn(facilitator, "--workers", "1")


@pytest.fixture
def preloaded(facilitator: MockFacilitator) -> Iterator[tuple[int, MockFacilitator]]:
    yield from start_gunicorn(facilitator, "--workers", "2", "--preload")


def offer(port: int) -> dict:  # type: ignore[type-arg]
    import base64
    import json

    response = httpx.get(f"http://127.0.0.1:{port}/paid")
    assert response.status_code == 402
    accepted: dict[str, Any] = json.loads(base64.b64decode(response.headers["payment-required"]))[
        "accepts"
    ][0]
    return accepted


def test_a_paid_request_with_a_signature_of_about_11_kib_is_served(
    gunicorn: tuple[int, MockFacilitator],
) -> None:
    port, facilitator = gunicorn
    header = payment_header(offer(port), tx_hash(1), preparedTransaction="A" * 8000)
    assert (
        len(header) > 9000
    )  # gunicorn's default limit_request_field_size (8190) would refuse this
    response = httpx.get(
        f"http://127.0.0.1:{port}/paid", headers={"PAYMENT-SIGNATURE": header}, timeout=30
    )
    assert response.status_code == 200, response.text
    assert response.json() == {"payer": facilitator.payer}


def test_a_payer_who_leaves_during_settle_on_a_sync_worker_is_served_once_on_retry(
    gunicorn: tuple[int, MockFacilitator],
) -> None:
    port, facilitator = gunicorn
    accepted = offer(port)
    facilitator.settle_queue.append(
        Answer(
            200,
            {
                "success": True,
                "transaction": "1220uGUNI",
                "network": "canton:mainnet",
                "payer": facilitator.payer,
            },
            delay=0.8,
        )
    )
    header = payment_header(accepted, tx_hash(7))
    request = f"GET /paid HTTP/1.1\r\nHost: x\r\nPAYMENT-SIGNATURE: {header}\r\nConnection: close\r\n\r\n".encode()
    sock = socket.create_connection(("127.0.0.1", port), timeout=30)
    sock.sendall(request)
    deadline = time.monotonic() + 15
    while facilitator.count("/settle") < 1 and time.monotonic() < deadline:
        time.sleep(0.01)
    sock.shutdown(socket.SHUT_RDWR)
    sock.close()
    time.sleep(1.2)  # the worker finishes the settle and keeps the body back
    retry = httpx.get(
        f"http://127.0.0.1:{port}/paid", headers={"PAYMENT-SIGNATURE": header}, timeout=30
    )
    assert retry.status_code == 200
    assert (
        facilitator.count("/settle") == 1
    )  # no second /settle: the recorded updateId was delivered
    again = httpx.get(
        f"http://127.0.0.1:{port}/paid", headers={"PAYMENT-SIGNATURE": header}, timeout=30
    )
    assert again.status_code == 402


def test_with_preload_each_worker_makes_its_own_facilitator_client_and_payments_do_not_cross(
    preloaded: tuple[int, MockFacilitator],
) -> None:
    """The master read /supported at import, so its pooled connection is inherited by both workers.
    Each worker must open its own: a shared keep-alive stream could hand one worker another's settle answer."""
    from concurrent.futures import ThreadPoolExecutor

    port, facilitator = preloaded
    seen: set[tuple[int, int, int]] = set()
    for _ in range(40):
        info = httpx.get(f"http://127.0.0.1:{port}/_info", timeout=10).json()
        seen.add((info["pid"], info["client_pid"], info["master"]))
        if len({pid for pid, _, _ in seen}) == 2:
            break
    assert len({pid for pid, _, _ in seen}) == 2, seen  # both workers answered
    for pid, client_pid, master in seen:
        assert (
            client_pid == pid and pid != master
        )  # the client of a worker was created by that worker

    accepted = offer(port)

    def pay(n: int) -> tuple[int, str]:
        header = payment_header(accepted, tx_hash(100 + n))
        response = httpx.get(
            f"http://127.0.0.1:{port}/paid", headers={"PAYMENT-SIGNATURE": header}, timeout=30
        )
        return response.status_code, response.text

    with ThreadPoolExecutor(8) as pool:
        results = list(pool.map(pay, range(16)))
    assert [code for code, _ in results] == [200] * 16, results
    assert facilitator.count("/settle") == 16
