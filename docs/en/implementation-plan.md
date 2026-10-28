# Implementation tracks

The binding project design is [architecture](01-architecture.md). Durable work
status lives in the project issue workflow; this document defines dependencies
and exit evidence, rather than a task ledger.

| Track | Scope | Exit evidence |
|---|---|---|
| T1 | Bounded Patroni probes and pure health intent policy | Health contract tests, container quality gates, independent plan/ADR/spec review |
| T2 | GoBGP ownership, safe UUID deletion, startup and ambiguous RPC reconciliation | Native foreign-route preservation and restart tests; independent plan/ADR/spec review |
| T3 | SQL pooler probe, supervised runtime and failure handling | Process crash/stall, live speaker, graceful restart and client outcomes; independent plan/ADR/spec review |
| T4 | Laboratory integration and operating-system comparison | Exact platform/storage identity, single-node then HA admission, measured failure and restore evidence |

T2 consumes T1 intent; it must check freshness at the actuation boundary.
T3 supplies real SQL health and supervised failure behavior. A container or unit
test does not qualify deployed database HA. T4 remains subject to exact target,
identity, credential and operational change gates.

No BFD or HAProxy dependency is introduced. Route withdrawal does not replace
Patroni/DCS/watchdog fencing. Private platform inventories and evidence stay
outside this public repository.
