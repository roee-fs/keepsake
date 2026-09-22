"""The admin session: password check, signed cookie, and the startup gate."""

from types import SimpleNamespace

import pytest
from starlette.exceptions import HTTPException
from starlette.requests import Request

from keepsake.server.auth import (
    COOKIE_NAME,
    Auth,
    MisconfiguredAdmin,
    admin_password,
    require_session,
    ui_enabled,
)

PASSWORD = "correct-password"


@pytest.fixture
def auth() -> Auth:
    return Auth(PASSWORD)


def test_the_right_password_is_accepted(auth: Auth) -> None:
    assert auth.check_password(PASSWORD)


def test_a_wrong_password_is_rejected(auth: Auth) -> None:
    assert not auth.check_password("wrong")


def test_a_freshly_issued_cookie_is_accepted(auth: Auth) -> None:
    assert auth.valid(auth.issue(ttl=3600))


def test_a_cookie_signed_with_another_password_is_rejected(auth: Auth) -> None:
    # The key derives from the password, so a password change MUST revoke old cookies.
    assert not Auth("old-password").valid(auth.issue(ttl=3600))


def test_an_expired_cookie_is_rejected(auth: Auth) -> None:
    assert not auth.valid(auth.issue(ttl=-1))


def test_admin_password_is_fatal_when_missing_and_ui_enabled(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.delenv("KEEPSAKE_ADMIN_PASSWORD", raising=False)
    monkeypatch.delenv("KEEPSAKE_UI", raising=False)
    with pytest.raises(MisconfiguredAdmin):
        admin_password()


def test_admin_password_is_fine_when_missing_and_ui_disabled(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.delenv("KEEPSAKE_ADMIN_PASSWORD", raising=False)
    monkeypatch.setenv("KEEPSAKE_UI", "false")
    assert not ui_enabled()
    assert admin_password() == ""


def _request(auth: Auth, cookie: str | None) -> Request:
    headers = [(b"cookie", f"{COOKIE_NAME}={cookie}".encode())] if cookie else []
    scope = {
        "type": "http",
        "headers": headers,
        "app": SimpleNamespace(state=SimpleNamespace(auth=auth)),
    }
    return Request(scope)


def test_require_session_rejects_a_missing_cookie(auth: Auth) -> None:
    with pytest.raises(HTTPException):
        require_session(_request(auth, None))


def test_require_session_accepts_a_valid_cookie(auth: Auth) -> None:
    require_session(_request(auth, auth.issue(ttl=3600)))
