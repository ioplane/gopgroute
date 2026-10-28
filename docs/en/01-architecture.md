# Architecture

Status: design, 2026-10-28. No daemon is implemented.

## Responsibility

GoPGRoute converts local PostgreSQL service eligibility into owned BGP paths.
The intended client path is a service VIP, local PgBouncer, then local
PostgreSQL. The adapter calls Patroni REST and the GoBGP API; it does not
replace Patroni, implement a DCS, elect a leader or fence PostgreSQL.

## Health decisions

| Service | Required observation |
|---|---|
| Write | Patroni `/primary` or `/read-write` returns HTTP 200; local pooler healthy |
| Read | Patroni `/replica?lag=<bound>` returns HTTP 200; local pooler healthy |
| Unknown, timeout or stale | Withdraw owned service path |

Use [Patroni REST semantics](https://patroni.readthedocs.io/en/latest/rest_api.html),
including replica load-balancing eligibility. Pin the supported Patroni and
GoBGP contracts before implementation. Configure request deadlines, response
limits, reconciliation interval and health freshness explicitly. A failed
callback does not disable periodic reconciliation.

## Route ownership and process failure

Only configured exact prefixes and adapter-owned paths may be changed.
Reconcile startup RIB state before advertising; a cached successful observation
cannot authorize indefinitely. Define safe recovery when the GoBGP API fails
and when an operation succeeds but its response is lost.

An adapter crash with a live speaker requires a separately validated bounded
withdrawal or session-stop mechanism. A normal shutdown hook alone cannot
handle SIGKILL or a frozen process. Keep APIs on loopback or authenticated,
authorized management transport. Never expose an unauthenticated route API.

[RFC 4724 section 4.2](https://www.rfc-editor.org/rfc/rfc4724.html#section-4.2)
allows a receiving peer to retain stale routes during graceful restart.
Peer configuration and measured stale-route duration therefore belong to the
failure contract. BGP withdrawal and DCS quorum do not substitute for fencing.

## Acceptance

Require deterministic tests for role changes, lag, health staleness, malformed
responses, API errors, startup reconciliation, ownership and ambiguous API
outcomes. Native integration must cover adapter crash and stall while GoBGP
stays alive, route cleanup after restart, peer graceful restart and absence of
changes to foreign routes. Measure client-visible failover and transaction
outcomes independently from API responses.

HA admission also requires Patroni/DCS/watchdog fencing, replication and
restore evidence. No BFD dependency or HAProxy datapath is part of this design.
