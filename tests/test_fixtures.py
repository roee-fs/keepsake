"""The DSN fixtures must hand tests an unprivileged role.

RLS is not enforced for superusers or table owners, so a privileged `pg_dsn`
would make every isolation test pass without proving anything.
"""

import psycopg
import pytest
from conftest import assert_role_unprivileged

_WHO_AM_I = (
    "SELECT current_user, "
    "has_database_privilege(current_user, current_database(), 'CREATE')"
)


def test_pg_dsn_is_the_unprivileged_role(
    pg_dsn: str, assert_not_privileged: None
) -> None:
    with psycopg.connect(pg_dsn) as conn:
        row = conn.execute(_WHO_AM_I).fetchone()
    assert row == ("okf_app", False)


def test_owner_dsn_can_create(owner_dsn: str) -> None:
    with psycopg.connect(owner_dsn) as conn:
        row = conn.execute(_WHO_AM_I).fetchone()
    assert row == ("okf_owner", True)


def test_guard_rejects_a_superuser(admin_dsn: str) -> None:
    with pytest.raises(AssertionError):
        assert_role_unprivileged(admin_dsn)
