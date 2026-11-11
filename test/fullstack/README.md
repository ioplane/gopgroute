# Full stack FRR lab

This Podman Compose fixture emulates three sites on one Linux host. Each site
has PostgreSQL, Patroni, an etcd voter, local PgBouncer and a GoPGRoute guardian
with its own dedicated upstream GoBGP process. Three FRR leaves exchange site
prefixes. A separate client reaches both VIPs through the kernel FIB; PowerDNS
uses the write VIP for its PostgreSQL backend. There are no published ports or
host-network containers.

Site networks are `192.0.2.0/26`, `198.51.100.0/26`, `203.0.113.0/26`;
transit is `198.18.0.0/24`, clients `198.19.0.0/24`. Write/read VIPs are
`198.19.255.10/32` and `198.19.255.11/32`. Reject existing host/container route
overlaps before starting. These networks must not connect to external peers.

## Preparation

Build the Containerfile from the repository root. Supply digest-pinned
`GO_IMAGE`, `PG_IMAGE` and `GOPROXY` from verified internal sources. Put the
approved Debian Trixie/security mirror configuration in ignored
`.runtime/internal.sources`. There is no public-source fallback. Preserve the
image ID, binary hashes and installed package versions with private evidence.
Supply `SOURCE_REVISION`, `BUILD_CREATED` (UTC RFC3339) and `PG_DIGEST` for
the image provenance labels.

Prepare a private env file containing `LAB_PROJECT`, `LAB_SESSION`, `LAB_IMAGE`
(the exact loaded image ID), `LAB_DATA`, `LAB_CREDENTIALS` and `LAB_CODE`.
Use a session-prefixed project. `LAB_CODE` contains the two Python files and is
mounted read-only; `LAB_DATA` holds three node directories and client evidence.
Create an empty private directory on host tmpfs for `LAB_CREDENTIALS`.
Run the image's `credentials` role once with that directory mounted read-write
at `/run/lab`. It generates a laboratory CA, three server/client certificates
and a random password. Never use production credentials or publish their
contents. Certificates expire after seven days; tmpfs credentials disappear
on host reboot.

Run `podman-compose` with `--in-pod false`,
`--podman-run-args=--image-volume=ignore`, `--env-file <private-env-file>`,
`-f test/fullstack/compose.yaml`, `-p <session-project>` and `up -d`.
The image-volume option avoids inherited anonymous PostgreSQL image volumes.
Verify actual container states: compose may return success after a failed
dependent-container removal. The init process reaps orphaned PostgreSQL
children; PID/socket directories use tmpfs. Only leaves receive the
`SYS_ADMIN` capability required by packaged zebra. PostgreSQL and route journals
persist in bind directories.

## Checks

Execute `/usr/bin/python3 /lab/check.py observe` inside the client container.
It checks both SQL roles over TLS `verify-full` and the uncached authoritative
`probe.ha.example.test` response. `receipt <integer>` commits an identifiable
transaction. `verify <acknowledged-id> ...` rejects any missing, additional or
changed receipt. Also run a negative case with an uncommitted identifier.

Inspect the FRR BGP RIB, kernel FIB and negotiated GR mode, PostgreSQL roles,
streaming/synchronous state and TLS, and three voting etcd members. Explicit
peer-level `graceful-restart-disable` is required: the global `no` command
alone leaves FRR in helper mode.

Inject faults only into this exact project: abrupt primary loss, suspended
PgBouncer, adapter SIGKILL/SIGSTOP and loss of the primary site's leaf. Measure
client recovery separately from withdrawal. During a partition, verify that
the isolated primary rejects new SQL connections; withdrawal alone is not
database fencing. Resume suspended processes before leaving the lab.

## Restart boundary

The guardian uses [inherited incarnation leases](../../docs/en/adr/0006-inherited-incarnation-leases.md)
and creates new journal/socket subdirectories on each start. Both children
retain both root locks; the next guardian must fail while either lease is
held. Old journals remain unchanged, including uncertain states. Restart the
whole route service with the same configuration; never restart a speaker alone.
The daemon has no internal respawn loop; this fixture does not add a restart policy.

Legacy flat roots require one explicit migration: prove the old route cgroup
empty, affected peers down with GR disabled and old next hops absent; preserve
and hash old journals, then provision empty private roots. The new protocol
cannot prove death of legacy processes that did not inherit its leases. Keep
at least the journal root and its lock inode stable across container recreation.
After migration, repeat clean stop/start and guardian SIGKILL/start with
unchanged roots. Verify distinct UUID journals, old bytes unchanged, both VIPs
in peer RIB/kernel FIB, client SQL/DNS and acknowledged transactions.

This single-host emulation does not qualify independent physical failure
domains, hardware watchdog fencing, storage failures, backup/PITR, production
TLS admission or multi-DC performance. Preserve the requested stand by its
persistence labels; cleanup must target only its exact compose project.
