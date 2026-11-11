# ADR 0006: inherited incarnation leases

Status: internal T3 recovery protocol; production admission remains separate.

## Decision

The guardian acquires stable exclusive nonblocking `guardian.lock` files in
both configured runtime and journal roots. The same open file descriptions
are inherited by both children: speaker fd 3/4, adapter fd 4/5 after progress
fd 3. They remain open through speaker exec. Close only the caller's copies;
never use `LOCK_UN`, which releases the shared lock prematurely.
[Linux flock](https://man7.org/linux/man-pages/man2/flock.2.html) defines this
lifetime; [Go ExtraFiles](https://pkg.go.dev/os/exec#Cmd) defines the FD mapping.

After acquiring both leases, create new mode-0700 `i-<128-bit-hex>` directories
in both roots. Synchronize lease files, new directories and parent roots
before starting children. Validate the expanded socket path. Sealed settings
bind both children to these directories. Each guardian starts a fresh speaker
and T2 manager; previous UUIDs never cross that boundary. Old directories,
journals and uncertain staging files remain unchanged. There is no current
pointer, journal reset, uncertain RPC retry or prefix deletion.

## Limits and migration

Keep both roots and lock inodes stable on a local filesystem. Replacing both
roots, restoring a copy while old children live, or ignoring the lease violates
admission. Changing one root still conflicts with the other lease. Never
delete, rename or explicitly unlock lock files. Roots contain only these
locks and generation directories; keep configuration and credentials outside.

Unknown root entries and legacy flat journals/sockets block startup. Migrating
the old protocol requires separately proven death of guardian, adapter and
speaker before provisioning empty roots. Old processes did not inherit these
leases; successful acquisition cannot retire them.

Partial preparation is retained and never reused. Generations accumulate;
retention requires a separate approved procedure. A failed process start or
stop does not release copies still held by another child. Kernel stalls,
peer GR, forwarding and database fencing remain separate acceptance gates.
The daemon does not respawn itself; a service manager restarts the complete
guardian with the same configuration.
