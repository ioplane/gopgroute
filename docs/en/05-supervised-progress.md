# Supervised process progress

Status: internal Linux components, not a daemon or a runtime release.

`supervisor.Run` owns two distinct unstarted direct child commands: one
dedicated GoBGP speaker and one adapter. The operator must exclude other
workloads and API writers from that speaker. Children must not daemonize or
detach descendants: the supervisor owns direct process handles, not a process
group or arbitrary PIDs. Standard streams must be nil or existing `*os.File`
objects. ExtraFiles, Cancel and SysProcAttr must be unset.

The supervisor creates a private pipe inherited only by the adapter at fd 3.
The adapter writes one `+` after a completed reconciliation cycle. A separate
heartbeat timer or an independent writer goroutine is forbidden: they could
keep routes alive while reconciliation or journal fsync is stalled.
`reconcile.Run` runs cycles serially, propagates errors and expired deadlines,
and emits progress only after success. The interval starts after completion;
missed intervals never queue catch-up cycles. A cycle must include the complete
health decision and all required route operations, including withdrawals on
unknown health. A callback that ignores its context can stall; the supervisor
process supplies the independent failure bound.

Any child exit, EOF, malformed heartbeat, missing progress or caller
cancellation stops both children. Shutdown sends SIGTERM to both owned handles,
waits up to StopTimeout, sends SIGKILL to remaining handles, then waits at most
another StopTimeout. Wait failures and an unreapable kernel task return errors.
Progress received after the preceding heartbeat deadline cannot renew the lease.
Uninterruptible kernel I/O can prevent actual reaping; do not start another
incarnation until the old children are proven dead. No automatic restart or
journal reset is provided. Keep the guardian scheduled independently of its
children; a stalled guardian cannot enforce a userspace deadline.

On Linux, Pdeathsig kills each child if its creating guardian thread dies.
Run keeps that OS thread locked while supervising and stopping its children.
Go process handles supply signaling authority; stored numeric PIDs do not.
These semantics are defined by the pinned Go toolchain's
[`SysProcAttr`](https://github.com/golang/go/blob/go1.27.3/src/syscall/exec_linux.go)
and [process handles](https://github.com/golang/go/blob/go1.27.3/src/os/pidfd_linux.go).

Real subprocess tests cover SIGKILL, SIGSTOP, ignored SIGTERM, startup failure,
broken progress and guardian death. Native GoBGP tests verify an advertised
route before adapter stall/uncertainty and termination of the dedicated speaker
afterward. They configure no BGP peer: peer graceful restart, RIB/FIB retention,
client outcomes, daemon configuration and Patroni/DCS/watchdog fencing remain
separate gates. Speaker shutdown is not proof of peer withdrawal or DB fencing.
