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

T4 qualification separates observation execution from the observed database
condition. Run potentially blocking SQL probes in an owned bounded worker;
timeout, cancellation and a kill request do not prove that worker stopped.
Retain its process incarnation when completion is unknown and refuse a
replacement probe or another fault until reconciliation. A completed worker
can still report an unknown database condition; re-observe only after confirmed
worker completion and within the original deadline. Before a primary-site
leaf fault, require a fresh baseline that places the primary at that site and
reject any unresolved preparation mutation. SQL demotion, fresh connection
refusal and route withdrawal are scoped observations; none proves durable
restart exclusion. Production admission additionally requires qualified
watchdog/external fencing and predecessor exclusion before successor writes.

Keep the current Patroni/GoPGRoute boundary. Experimental request-ordering,
physical-work accounting and WAL-history comparators are useful specification
inputs; pure state-machine tests do not qualify their adapters or provide a
deployable replacement HA controller. Prioritize physical fencing evidence,
then proven replay-lag injection, then recovery-budget diagnosis. Task status
and exact private runtime receipts remain in the existing issue workflow.

Qualify the explicit laboratory replication timeout profile separately from
the former defaults. Require effective values on every node, primary-site
loss and DCS-partition transactions, exact ledger preservation and recovery.
A network-connection timeout is not a general process-stop deadline or
independent watchdog; production predecessor exclusion remains a separate gate.

Replay-lag qualification must hold replay while preserving the WAL receive and
durable-flush path. Bind the stopped startup incarnation to SQL and its
postmaster; take the replay baseline after confirmed STOP. Prove the same
replication connection and synchronous membership before and after a client
ACK, then restore and verify the exact ledger. SQL replay pause alone does
not qualify a sustained fault when the HA controller automatically resumes it.
