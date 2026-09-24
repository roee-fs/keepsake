---
type: Runbook
title: Rotate TLS certificates
description: Renew and roll the gateway's certificates.
tags: [edge, tls]
owner: edge
---
Renewal is automatic thirty days before expiry. If it fails, renew by hand and
roll the [API gateway](/services/api-gateway.md) one zone at a time. The alert
fires fourteen days out since [the July expiry](/incidents/2026-07-cert-expiry.md).
