# Changelog

All notable changes are documented here using [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- Replay-lag injection holds only the verified startup process and proves
  durable WAL progress with unchanged replay and synchronous membership.
  Client receipts explicitly use synchronous commit; rollback waits for the
  exact injector to finish before resuming the original process incarnation.

- The isolated fixture uses explicit 10-second replication timeouts and checks
  their effective values before faults. The bounded configuration operation
  verifies DCS readback and supports restoring the former 60-second timers.

- Primary-site leaf failures are explicitly prepared as primary losses.
  Transient unknown SQL results may be re-observed inside the original window
  only after worker completion and with a full worker transport budget;
  unresolved workers still block the campaign.

- SQL fencing observations run in a bounded owned worker. A frozen or failed
  worker reports an unknown database outcome; unconfirmed worker completion
  blocks replacement probes and retains campaign uncertainty.

- Replay-lag faults emit bounded WAL payloads and require a measured lag above
  the configured threshold before asserting route withdrawal.

- Laboratory COMMIT observations use a separate 45-second acknowledgement
  bound instead of truncating synchronous-standby replacement at 15 seconds;
  unknown outcomes still stop without replay.

- Unchanged route intent requires bounded read-only speaker progress before
  renewing the adapter heartbeat; a frozen speaker stops reconciliation.

- Whole-guardian restart uses inherited runtime/journal leases and fresh
  incarnation directories instead of replaying old speaker UUIDs. Previous
  journals remain intact; legacy flat state requires explicit migration.
  Lab route containers retry failed guardian exits up to three times.

### Security

- Go 1.27.4 rebuilds runtime and SQL fixtures with the standard-library fix for
  GO-2026-6859. Runtime toolchain selection refuses download fallback; the
  independently pinned formatter/linter retains its own analysis compiler.

### Added

- A finite 57-case GoBGP/Patroni fault campaign with bounded remote execution,
  exact isolated-lab admission, registered recovery, transaction/journal
  checks and private JSON/Markdown reports. Expected limitations and blocked
  cases remain distinct from passing invariants; ambiguous mutations are not
  replayed. Python probe/fault unit tests run in the existing Go container.

- Isolated Podman Compose full stack fixture with three PostgreSQL/Patroni
  sites, etcd, PgBouncer, supervised GoBGP, FRR leaves and a routed SQL/PowerDNS
  client. Checks distinguish role eligibility and acknowledged transactions;
  persistent speaker restart remains a separate acceptance gate.

- Linux daemon candidate with sealed child settings, a digest-pinned external
  speaker, exec-thread parent-death protection, read-only policy admission and
  role-bound SQL cycles. Configuration fuzzing, exhaustive role combinations,
  exec/guardian-death and native PostgreSQL/PgBouncer RIB tests qualify source
  behavior; artifacts, restart recovery and peer/client HA remain separate.
  Format/lint use the official pinned container image.

- Linux supervisor for a dedicated speaker/adapter process pair, private fd-3
  progress, bounded TERM/KILL shutdown, child-incarnation binding and parent
  death signals. Serial reconciliation emits progress only after success;
  process and native GoBGP fault tests cover crash, stall and uncertainty.
  Daemon wiring, peer graceful restart and database HA admission remain separate.

- Bounded SQL observation through the local PgBouncer with SCRAM-only
  authentication, a required initial SASL request, exact backend identity,
  ADR 0003 and native PostgreSQL 18.6 / PgBouncer 1.26.1 tests.
- Private Unix GoBGP actuator with durable UUID ownership, exact withdrawal,
  startup refusal on uncertainty and native lost-response/restart tests.
- Route ownership contract and ADR 0002 in English and Russian; runtime
  admission remains dependent on independent speaker supervision.
- Bounded Patroni HTTP/TLS probes, strict health-response validation and a
  freshness-based intent policy, with container race and security checks.
- Health contract, ADR 0001 and implementation tracks in English and Russian.

- Public project documentation, naming convention, contribution policies and
  repository checks for the Patroni-to-GoBGP adapter design.
