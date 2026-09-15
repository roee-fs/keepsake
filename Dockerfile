FROM ghcr.io/astral-sh/uv:python3.14-bookworm-slim AS build
ENV UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy
WORKDIR /app
COPY pyproject.toml uv.lock README.md ./
COPY src ./src
# --no-editable so the venv carries the package rather than a link into /app/src.
RUN uv sync --locked --no-dev --no-editable

FROM python:3.14-slim-bookworm
# psycopg ships a binary wheel, so the runtime needs no libpq.
COPY --from=build /app/.venv /app/.venv
ENV PATH=/app/.venv/bin:$PATH
USER nobody
CMD ["keepsake", "serve"]
