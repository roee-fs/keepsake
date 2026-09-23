"""The admin session: one password, one signed cookie, no server-side state."""

import hashlib
import hmac
import os
import time

from starlette.exceptions import HTTPException
from starlette.requests import Request

COOKIE_NAME = "keepsake_session"


def _utf8(value: str) -> bytes:
    """Encode for `compare_digest`, which raises TypeError on a non-ASCII `str`.

    surrogatepass, so an unpaired surrogate out of a JSON body encodes rather than
    raising on the one unauthenticated route.
    """
    return value.encode("utf-8", "surrogatepass")


class MisconfiguredAdmin(RuntimeError):
    """Raised at startup. An enabled console with no password is worse than none."""


def ui_enabled() -> bool:
    """Whether the admin console is on. Defaults to on."""
    return os.environ.get("KEEPSAKE_UI", "true").lower() == "true"


def admin_password() -> str:
    """Read KEEPSAKE_ADMIN_PASSWORD, refusing to start if the console is on without one."""
    # A password input cannot submit a newline, and `echo pw | base64` appends one.
    password = os.environ.get("KEEPSAKE_ADMIN_PASSWORD", "").rstrip("\r\n")
    if ui_enabled() and not password:
        raise MisconfiguredAdmin(
            "KEEPSAKE_ADMIN_PASSWORD is unset but the admin console is enabled"
        )
    return password


class Auth:
    """The one admin account. The password is the whole credential."""

    def __init__(self, password: str) -> None:
        self._password = password
        # Derived, so changing the password revokes every cookie. scrypt, because a
        # leaked cookie lets anyone test password guesses offline at the key's cost.
        self._key = hashlib.scrypt(
            _utf8(password), salt=b"keepsake-session", n=2**14, r=8, p=1, dklen=32
        )

    def check_password(self, supplied: str) -> bool:
        # compare_digest, not ==: == leaks the matching prefix length through timing.
        return hmac.compare_digest(_utf8(supplied), _utf8(self._password))

    def issue(self, ttl: int) -> str:
        expiry = int(time.time()) + ttl
        digest = hmac.new(self._key, str(expiry).encode(), hashlib.sha256).hexdigest()
        return f"{expiry}.{digest}"

    def valid(self, cookie: str) -> bool:
        expiry, _, digest = cookie.partition(".")
        if not expiry.isdecimal():
            return False
        expected = hmac.new(self._key, expiry.encode(), hashlib.sha256).hexdigest()
        return (
            hmac.compare_digest(_utf8(digest), _utf8(expected))
            and int(expiry) >= time.time()
        )


def require_session(request: Request) -> None:
    """FastAPI dependency: 401s unless the request carries a valid session cookie."""
    auth: Auth = request.app.state.auth
    cookie = request.cookies.get(COOKIE_NAME)
    if cookie is None or not auth.valid(cookie):
        raise HTTPException(status_code=401)
