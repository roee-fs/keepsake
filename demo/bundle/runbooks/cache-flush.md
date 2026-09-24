---
type: Runbook
title: Cache flush
description: Flush Valkey without a stampede.
tags: [valkey]
owner: data-platform
---
Flush [Valkey](/services/valkey.md) one key prefix at a time, never with
`FLUSHALL`. Scale the backend up first with [scale workers](/runbooks/scale-workers.md).
Written after [the May stampede](/incidents/2026-05-cache-stampede.md).
