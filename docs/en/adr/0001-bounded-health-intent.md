# ADR 0001: bounded health observations before route actuation

Date: 2026-10-28. Status: accepted for T1.

## Context

The accepted architecture separates Patroni authority, pooler health and BGP
actuation. A status-only TCP check, stale cached success, a redirect to another
health server, or ambiguous JSON can incorrectly authorize an announcement.
Independent finite observations are also susceptible to a role transition
between requests. T1 must be useful and testable without modifying any route.

## Decision

Implement internal Patroni HTTP/TLS probes and a pure intent policy according
to [the health contract](../02-health-contract.md). Use only Go standard-library
runtime packages. Retain request-start time, require freshness at evaluation,
and deny both intents when two fresh role checks are positive. Keep deadlines,
body/header limits, TLS trust and URL restrictions inside the probe boundary.

HTTP is limited to literal loopback endpoints. HTTPS requires verified CA and
hostname and TLS 1.3. There is no insecure-verification switch. Fail closed on
ambiguous consumed JSON fields without forbidding unrelated response fields.

Keep route ownership, durable reconciliation, SQL pooler checks and process
supervision in subsequent tracks. Do not expose a runnable daemon or a release
until those safety requirements have executable evidence.

## Consequences

A false negative can temporarily withdraw an intent, including during role
changes. An observation expires while a slow request or later operation runs;
its start time is deliberately conservative. An endpoint restricted to TLS 1.2
will be rejected. This choice requires endpoint modernization rather than a
verification bypass. Health intent alone gives no fencing or RPO/RTO guarantee.

## Verification

The contract maps to real HTTP/TLS tests and deterministic three-state/freshness
policy tests. Each delivery track receives an independent comparison of its
implementation, test evidence, plan, this ADR and the binding specification.
