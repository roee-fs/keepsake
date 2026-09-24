---
type: Service
title: Valkey
description: Shared cache and job queue.
tags: [datastore]
owner: data-platform
---
Owned by [data platform](/teams/data-platform.md). Caches session data for the
[auth service](/services/auth-service.md) and queues jobs for the
[billing worker](/services/billing-worker.md).

A full flush logs everyone out and drops queued billing jobs. Follow
[cache flush](/runbooks/cache-flush.md) and read
[the May stampede](/incidents/2026-05-cache-stampede.md) first.
