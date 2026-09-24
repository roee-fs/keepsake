---
type: Runbook
title: Scale workers
description: Add backend and billing capacity by hand.
tags: [capacity]
---
Raise the replica count on the backend or the
[billing worker](/services/billing-worker.md). Watch [Postgres](/services/postgres.md)
connections; each worker holds four.
