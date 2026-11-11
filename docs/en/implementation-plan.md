# Implementation tracks

The binding project design is [architecture](01-architecture.md). Durable work
status lives in the project issue workflow; this document defines dependencies
and exit evidence, rather than a task ledger.

| Track | Scope | Exit evidence |
|---|---|---|
| T1 | Bounded Patroni probes and pure health intent policy | Health contract tests, container quality gates, independent plan/ADR/spec review |
| T2 | GoBGP ownership, safe UUID deletion, startup and ambiguous RPC reconciliation | Native foreign-route preservation and restart tests; independent plan/ADR/spec review |
| T3 | SQL pooler probe, supervised runtime and failure handling | Process crash/stall, speaker-incarnation binding, graceful restart and client outcomes; independent plan/ADR/spec review |
| T4 | Laboratory integration and operating-system comparison | Exact platform/storage identity, single-node then HA admission, measured failure and restore evidence |

T2 consumes T1 intent; it must check freshness at the actuation boundary.
T3 supplies real SQL health and supervised failure behavior. A container or unit
test does not qualify deployed database HA. T4 remains subject to exact target,
identity, credential and operational change gates.

T3 consists of subtracks, each with an independent plan/ADR/spec review:

| Subtrack | Scope | Exit evidence |
|---|---|---|
| T3.1 | SQL through actual local PgBouncer | Native SCRAM, role/backend identity, protocol and timeout tests |
| T3.2 | Independent guardian and completed-cycle progress | Real crash/stall, native route uncertainty and bounded stop |
| T3.3 | Daemon, sealed settings, pinned external speaker | Config/fuzz, role matrix, parent death across exec, native SQL/RIB lifecycle and container quality |
| T3.4 | Artifact, restart/recovery, peers, client outcomes | Exact artifact/advisory qualification, clean incarnation recovery, measured GR/RIB/FIB retention and transactions |

The daemon remains a source candidate. An open PR and green CI do not close
T3.4 or authorize T4. The [daemon contract](06-daemon-contract.md) and
[ADR 0005](adr/0005-sealed-daemon-incarnation.md) bind the integration.
[ADR 0006](adr/0006-inherited-incarnation-leases.md) binds restart recovery:
require lease retention across speaker exec, refusal while either child holds
a lease, unchanged old journals and fresh-UUID native SQL announcement after
clean shutdown and guardian SIGKILL. Qualify peer RIB/FIB and transactions
separately from process proof.

No BFD or HAProxy dependency is introduced. Route withdrawal does not replace
Patroni/DCS/watchdog fencing. Private platform inventories and evidence stay
outside this public repository.
