"""The admin session: one password, one signed cookie, no server-side state."""

import hashlib
import hmac
import os
import time

from starlette.exceptions import HTTPException
from starlette.requests import Request

COOKIE_NAME = "keepsake_session"


class MisconfiguredAdmin(RuntimeError):
    """Raised at startup. An enabled console with no password is worse than none."""


def ui_enabled() -> bool:
    """Whether the admin console is on. Later tasks gate their route/static mounts on this."""
    return os.environ.get("KEEPSAKE_UI", "true").lower() == "true"


def admin_password() -> str:
    """Read KEEPSAKE_ADMIN_PASSWORD, refusing to start if the console needs one and lacks it."""
    password = os.environ.get("KEEPSAKE_ADMIN_PASSWORD", "")
    if ui_enabled() and not password:
        raise MisconfiguredAdmin(
            "KEEPSAKE_ADMIN_PASSWORD is unset but the admin console is enabled"
        )
    return password


class Auth:
    """The one admin account. The password is the whole credential."""

    def __init__(self, password: str) -> None:
        self._password = password
        # Derived rather than a second secret: changing the password changes the key,
        # so every outstanding cookie stops verifying. Argo CD spends an
        # admin.passwordMtime field to get the same effect.
        self._key = hmac.new(
            password.encode(), b"keepsake-session", hashlib.sha256
        ).digest()

    def check_password(self, supplied: str) -> bool:
        # compare_digest, not ==: the obvious comparison leaks the length of the
        # matching prefix through timing.
        return hmac.compare_digest(supplied, self._password)

    def issue(self, ttl: int) -> str:
        expiry = int(time.time()) + ttl
        digest = hmac.new(self._key, str(expiry).encode(), hashlib.sha256).hexdigest()
        return f"{expiry}.{digest}"

    def valid(self, cookie: str) -> bool:
        expiry, _, digest = cookie.partition(".")
        if not expiry.isdecimal():
            return False
        expected = hmac.new(self._key, expiry.encode(), hashlib.sha256).hexdigest()
        return hmac.compare_digest(digest, expected) and int(expiry) >= time.time()


def require_session(request: Request) -> None:
    """FastAPI dependency: 401s unless the request carries a valid session cookie."""
    auth: Auth = request.app.state.auth
    cookie = request.cookies.get(COOKIE_NAME)
    if cookie is None or not auth.valid(cookie):
        raise HTTPException(status_code=401)
