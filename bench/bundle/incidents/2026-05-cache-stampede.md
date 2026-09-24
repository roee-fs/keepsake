---
type: Incident
title: Cache stampede
description: A full Valkey flush took the backend down for twelve minutes.
tags: [valkey, sev1]
---
Someone ran `FLUSHALL` on [Valkey](/services/valkey.md) to clear a bad key. Every
request missed the cache at once and saturated [Postgres](/services/postgres.md).
Led to the [cache flush](/runbooks/cache-flush.md) runbook.
