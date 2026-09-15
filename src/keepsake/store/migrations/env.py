"""Alembic environment. Keeps alembic's version table inside the okf schema."""

from alembic import context
from sqlalchemy import create_engine, text
from sqlalchemy.engine.url import make_url

SCHEMA = "okf"


def _url() -> str:
    """Name the driver, or SQLAlchemy reads a bare postgresql:// DSN as psycopg2."""
    url = make_url(context.config.get_main_option("sqlalchemy.url", ""))
    return url.set(drivername="postgresql+psycopg").render_as_string(
        hide_password=False
    )


with create_engine(_url()).connect() as connection:
    # Alembic creates its version table but never the schema holding it.
    connection.execute(text(f"CREATE SCHEMA IF NOT EXISTS {SCHEMA}"))
    connection.commit()
    context.configure(connection=connection, version_table_schema=SCHEMA)
    with context.begin_transaction():
        context.run_migrations()
