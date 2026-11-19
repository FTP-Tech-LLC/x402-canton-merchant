from __future__ import annotations

import logging
from collections.abc import Callable, Iterator
from typing import Any

import pytest

from tests.mock_facilitator import MockFacilitator
from tests.support import Harness


@pytest.fixture
def facilitator() -> Iterator[MockFacilitator]:
    mock = MockFacilitator().start()
    yield mock
    mock.stop()


@pytest.fixture(params=["asgi", "wsgi"])
def kind(request: pytest.FixtureRequest) -> str:
    return str(request.param)


@pytest.fixture
def make(kind: str, facilitator: MockFacilitator) -> Iterator[Callable[..., Harness]]:
    """Build harnesses for both drivers: ``make(store=..., settle_calls=2)``."""
    built: list[Harness] = []

    def factory(**config: Any) -> Harness:
        harness = Harness(kind, facilitator, **config)
        built.append(harness)
        return harness

    yield factory
    for harness in built:
        harness.close()


@pytest.fixture
def harness(make: Callable[..., Harness]) -> Harness:
    return make()


@pytest.fixture(autouse=True)
def _quiet_logs(caplog: pytest.LogCaptureFixture) -> Iterator[None]:
    caplog.set_level(logging.DEBUG, logger="ftptech_x402_canton")
    yield


@pytest.fixture(autouse=True)
def _no_leaked_request_state() -> Iterator[None]:
    """Unit tests set the current request state by hand; none may leak into the next test."""
    from ftptech_x402_canton.state import CURRENT_STATE

    CURRENT_STATE.set(None)
    yield
    CURRENT_STATE.set(None)
