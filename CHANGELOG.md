# Changelog

All notable changes are documented here using [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

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
