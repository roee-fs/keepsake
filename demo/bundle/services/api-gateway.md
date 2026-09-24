---
type: Service
title: API gateway
description: Terminates TLS and routes every public request.
tags: [edge, tier-0]
owner: edge
---
Owned by [edge](/teams/edge.md). Terminates TLS with certificates renewed by
[rotate TLS certificates](/runbooks/rotate-tls-certs.md).

Routes to the [auth service](/services/auth-service.md) first, then to the
backend. Its p99 latency is the headline SLO and spends the
[error budget](/glossary/error-budget.md) fastest.
