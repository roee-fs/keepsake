FROM ghcr.io/astral-sh/uv:python3.14-bookworm-slim AS build
ENV UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy
WORKDIR /app
# LICENSE as well: `license-files` in pyproject makes it part of the build, and the
# backend refuses outright when the glob matches nothing.
COPY pyproject.toml uv.lock README.md LICENSE ./
COPY src ./src
# --no-editable so the venv carries the package rather than a link into /app/src.
RUN uv sync --locked --no-dev --no-editable

FROM python:3.14-slim-bookworm
# psycopg ships a binary wheel, so the runtime needs no libpq.
COPY --from=build /app/.venv /app/.venv
ENV PATH=/app/.venv/bin:$PATH
# Numeric, not the name `nobody`: the kubelet cannot resolve a name to a UID, so a
# pod asking for runAsNonRoot refuses to start rather than running unprivileged.
USER 65534
CMD ["keepsake", "serve"]
