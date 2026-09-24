---
type: Incident
title: API latency spike
description: Gateway p99 tripled for forty minutes after a config push.
tags: [edge, sev2]
---
A routing change on the [API gateway](/services/api-gateway.md) sent every request
through the [auth service](/services/auth-service.md) twice. Burned a third of the
month's [error budget](/glossary/error-budget.md). Rolled back.
