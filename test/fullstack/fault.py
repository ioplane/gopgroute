# /// script
# requires-python = ">=3.13"
# dependencies = ["psycopg2-binary==2.9.10"]
# ///
"""Closed fault operations for the isolated fixture; never execute a shell."""
# ruff: noqa: PLR2004, TRY003, PLC0415
# Fixed fixture constants/errors stay literal; the SQL-only dependency is lazy.

from __future__ import annotations

import json
import http.client
import fcntl
import os
import re
import signal
import stat
import sys
from collections.abc import Generator
from contextlib import closing, contextmanager
from pathlib import Path

ALLOWED_SIGNALS = (signal.SIGSTOP, signal.SIGCONT, signal.SIGKILL)
EVIDENCE = Path("/evidence")
STATE = Path("/state")
MANIFEST_LIMIT = 128 * 1024
SQL = {
    "pause": "SELECT pg_wal_replay_pause()",
    "resume": "SELECT pg_wal_replay_resume()",
    "wal": (
        "SELECT pg_logical_emit_message(false, 'gopgroute-lab', "
        "repeat('x', 2097152), true)::text"
    ),
}


@contextmanager
def deadline(seconds: int) -> Generator[None]:
    def expired(_signum: int, _frame: object) -> None:
        raise TimeoutError("Fault API deadline exceeded; outcome may be uncertain")

    previous = signal.signal(signal.SIGALRM, expired)
    signal.setitimer(signal.ITIMER_REAL, seconds)
    try:
        yield
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous)


def validate_identity(pid: int, start: str) -> None:
    if not isinstance(pid, int) or isinstance(pid, bool) or pid <= 1:
        raise ValueError("PID must identify a container child")
    if (
        not isinstance(start, str)
        or not start.isascii()
        or not start.isdecimal()
        or len(start) > 20
    ):
        raise ValueError("Invalid process start identity")


def identity(pid: int) -> str:
    validate_identity(pid, "0")
    data = Path(f"/proc/{pid}/stat").read_text(encoding="utf-8")
    if not data.startswith(f"{pid} ("):
        raise ValueError("Process stat PID mismatch")
    tail = data.rsplit(") ", 1)
    if len(tail) != 2 or len(tail[1].split()) < 20:
        raise ValueError("Invalid process stat")
    start = tail[1].split()[19]
    validate_identity(pid, start)
    return start


def send(pid: int, start: str, signum: signal.Signals) -> dict:
    validate_identity(pid, start)
    if not isinstance(signum, signal.Signals) or signum not in ALLOWED_SIGNALS:
        raise ValueError("Unsupported fault signal")
    gone = {"gone": True, "pid": pid, "start_time": start}
    try:
        actual = identity(pid)
    except (FileNotFoundError, ProcessLookupError):
        if signum == signal.SIGCONT:
            return gone
        raise
    if actual != start:
        if signum == signal.SIGCONT:
            return gone
        raise ValueError("Process incarnation changed")
    try:
        fd = os.pidfd_open(pid)
    except ProcessLookupError:
        if signum == signal.SIGCONT:
            return gone
        raise
    try:
        try:
            if identity(pid) != start:
                if signum == signal.SIGCONT:
                    return gone
                raise ValueError("Process incarnation changed while opening pidfd")
            signal.pidfd_send_signal(fd, signum)
        except (FileNotFoundError, ProcessLookupError):
            if signum == signal.SIGCONT:
                return gone
            raise
    finally:
        os.close(fd)
    return {"pid": pid, "start_time": start, "signal": signum.name}


def validate_processes(items: object, *, empty: bool = False) -> list[dict]:
    if not isinstance(items, list) or (not items and not empty) or len(items) > 1000:
        raise ValueError("Invalid paused process identities")
    for item in items:
        if not isinstance(item, dict) or set(item) != {"pid", "start_time"}:
            raise ValueError("Invalid paused process identity fields")
        validate_identity(item["pid"], item["start_time"])
    if len({item["pid"] for item in items}) != len(items):
        raise ValueError("Duplicate paused process identity")
    return items


def pg_resume(items: list[dict]) -> list[dict]:
    validate_processes(items)
    results = []
    failures = []
    for item in reversed(items):
        try:
            results.append(send(item["pid"], item["start_time"], signal.SIGCONT))
        # Continue rollback after interruption, then propagate the aggregate.
        except BaseException as exc:  # noqa: BLE001
            failures.append(exc)
    if failures:
        raise BaseExceptionGroup("Cannot restore paused PostgreSQL processes", failures)
    return results


def pause_paths(nonce: str) -> tuple[Path, Path, Path]:
    if re.fullmatch(r"[0-9a-f]{32}", nonce) is None:
        raise ValueError("Invalid pause token")
    path = STATE / f"lab-pg-pause-{nonce}.json"
    return path, path.with_suffix(".json.next"), path.with_suffix(".json.guard")


def present(path: Path) -> bool:
    try:
        path.lstat()
    except FileNotFoundError:
        return (
            False  # fail-silent-ok: ENOENT is the explicit absent-intent observation.
        )
    return True


def private_manifest(descriptor: int, path: Path) -> None:
    info, linked = os.fstat(descriptor), path.lstat()
    if (
        not stat.S_ISREG(info.st_mode)
        or stat.S_IMODE(info.st_mode) != 0o600
        or info.st_uid != os.geteuid()
        or info.st_nlink != 1
        or info.st_size > MANIFEST_LIMIT
        or (info.st_dev, info.st_ino) != (linked.st_dev, linked.st_ino)
    ):
        raise ValueError("Invalid pause manifest identity")


def sync_state() -> None:
    descriptor = os.open(STATE, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


@contextmanager
def pause_guard(nonce: str, *, create: bool) -> Generator[tuple[Path, Path]]:
    path, staging, guard = pause_paths(nonce)
    if create and any(present(item) for item in (path, staging, guard)):
        raise FileExistsError("Pause token already exists; STOP must not be replayed")
    flags = os.O_RDWR | os.O_NOFOLLOW | os.O_NONBLOCK
    descriptor = os.open(
        guard, flags | (os.O_CREAT | os.O_EXCL if create else 0), 0o600
    )
    try:
        private_manifest(descriptor, guard)
        fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if create:
            os.fsync(descriptor)
            sync_state()
        yield path, staging
    finally:
        os.close(descriptor)
    # The guard remains a nonce tombstone, preventing reuse after cleanup.


def save_pause(path: Path, staging: Path, value: dict, *, create: bool) -> None:
    data = (json.dumps(value, separators=(",", ":"), sort_keys=True) + "\n").encode()
    if len(data) > MANIFEST_LIMIT:
        raise ValueError("Pause manifest exceeds limit")
    destination = path if create else staging
    descriptor = os.open(
        destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600
    )
    with os.fdopen(descriptor, "wb") as stream:
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())
    if not create:
        staging.replace(path)
    sync_state()


def private_json(path: Path) -> object:
    def unique(items: list[tuple[str, object]]) -> dict:
        if len(dict(items)) != len(items):
            raise ValueError("Duplicate pause manifest field")
        return dict(items)

    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(descriptor, "rb") as stream:
        private_manifest(stream.fileno(), path)
        data = stream.read(MANIFEST_LIMIT + 1)
        if len(data) > MANIFEST_LIMIT:
            raise ValueError("Pause manifest exceeds limit")
        value = json.loads(data, object_pairs_hook=unique)
    return value


def load_pause(path: Path, nonce: str) -> dict:
    value = private_json(path)
    if (
        not isinstance(value, dict)
        or set(value) != {"version", "nonce", "processes"}
        or not isinstance(value["version"], int)
        or isinstance(value["version"], bool)
        or value["version"] != 1
        or value["nonce"] != nonce
    ):
        raise ValueError("Invalid pause manifest schema")
    validate_processes(value["processes"], empty=True)
    return value


def clear_pause(path: Path, staging: Path) -> None:
    if present(staging):
        descriptor = os.open(staging, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        try:
            private_manifest(descriptor, staging)
        finally:
            os.close(descriptor)
        # A staging update precedes rename/fsync and therefore precedes its STOP.
        staging.unlink()
    path.unlink()
    sync_state()


def pg_stop(nonce: str) -> dict:
    with pause_guard(nonce, create=True) as (path, staging):
        root = int(
            (STATE / "pg/postmaster.pid").read_text(encoding="utf-8").splitlines()[0]
        )
        stopped: list[dict] = []
        value = {"version": 1, "nonce": nonce, "processes": stopped}
        save_pause(path, staging, value, create=True)
        todo = [root]
        try:
            while todo:
                pid = todo.pop()
                start = identity(pid)
                if len(stopped) >= 1000 or any(item["pid"] == pid for item in stopped):
                    raise ValueError("Invalid PostgreSQL process tree")  # noqa: TRY301 # Validate inside rollback protection.
                stopped.append({"pid": pid, "start_time": start})
                save_pause(path, staging, value, create=False)
                send(pid, start, signal.SIGSTOP)
                todo.extend(
                    int(x)
                    for x in Path(f"/proc/{pid}/task/{pid}/children")
                    .read_text(encoding="utf-8")
                    .split()
                )
        except (
            BaseException
        ) as cause:  # Mutation cleanup also restores after interruption.
            try:
                if stopped:
                    pg_resume(stopped)
                clear_pause(path, staging)
            except BaseException as cleanup:  # noqa: BLE001 # Preserve every rollback failure.
                raise BaseExceptionGroup(
                    "PostgreSQL stop and rollback failed", [cause, cleanup]
                ) from None
            raise
    return {"stopped": len(stopped), "token": nonce}


def pg_resume_token(nonce: str) -> dict:
    path, staging, guard = pause_paths(nonce)
    if not present(guard):
        if present(path) or present(staging):
            raise ValueError("Pause intent lacks its serialization guard")
        return {"no_pending_intent": True}
    with pause_guard(nonce, create=False):
        if not present(path):
            if present(staging):
                raise ValueError("Orphan pause staging file")
            return {"no_pending_intent": True}
        value = load_pause(path, nonce)
        results = pg_resume(value["processes"]) if value["processes"] else []
        clear_pause(path, staging)
    return {"resumed": True, "processes": results}


def signal_path(nonce: str) -> Path:
    if re.fullmatch(r"[0-9a-f]{32}", nonce) is None:
        raise ValueError("Invalid signal token")
    return STATE / f"lab-signal-{nonce}.json"


def signal_token(nonce: str, pid: int, start: str, signum: signal.Signals) -> dict:
    validate_identity(pid, start)
    if not isinstance(signum, signal.Signals) or signum not in ALLOWED_SIGNALS:
        raise ValueError("Unsupported token signal")
    path = signal_path(nonce)
    staging = path.with_suffix(".json.next")
    if present(path) or present(staging):
        raise FileExistsError(
            "Signal token exists; uncertain mutation must not be replayed"
        )
    injector = os.getpid()
    value = {
        "version": 1,
        "nonce": nonce,
        "phase": "prepared",
        "target": {"pid": pid, "start_time": start, "signal": signum.name},
        "injector": {
            "pid": injector,
            "start_time": identity(injector),
            "pid_namespace_inode": Path("/proc/self/ns/pid").stat().st_ino,
        },
    }
    save_pause(path, staging, value, create=True)
    result = send(pid, start, signum)
    value["phase"] = "returned"
    save_pause(path, staging, value, create=False)
    return {"token": nonce, "phase": "returned", "result": result}


def signal_status(nonce: str) -> dict:
    path = signal_path(nonce)
    if not present(path):
        return {
            "present": False,
            "injector_quiescent": False,
            "effect_confirmed": False,
        }
    value = private_json(path)
    if (
        not isinstance(value, dict)
        or set(value) != {"version", "nonce", "phase", "target", "injector"}
        or not isinstance(value["version"], int)
        or isinstance(value["version"], bool)
        or value["version"] != 1
        or value["nonce"] != nonce
        or value["phase"] not in ("prepared", "returned")
    ):
        raise ValueError("Invalid signal marker schema")
    target, injector = value["target"], value["injector"]
    if (
        not isinstance(target, dict)
        or set(target) != {"pid", "start_time", "signal"}
        or target["signal"] not in {item.name for item in ALLOWED_SIGNALS}
        or not isinstance(injector, dict)
        or set(injector) != {"pid", "start_time", "pid_namespace_inode"}
    ):
        raise ValueError("Invalid signal process identities")
    validate_identity(target["pid"], target["start_time"])
    validate_identity(injector["pid"], injector["start_time"])
    namespace = injector["pid_namespace_inode"]
    if not isinstance(namespace, int) or isinstance(namespace, bool) or namespace <= 0:
        raise ValueError("Invalid injector namespace identity")
    state = "namespace_changed"
    if Path("/proc/self/ns/pid").stat().st_ino == namespace:
        try:
            observed = identity(injector["pid"])
            if observed != injector["start_time"]:
                state = "reused"
            else:
                fields = (
                    Path(f"/proc/{injector['pid']}/stat")
                    .read_text(encoding="utf-8")
                    .rsplit(") ", 1)[1]
                    .split()
                )
                if len(fields) < 20:
                    raise ValueError("Invalid injector process stat")
                # Retain zombie as an observation; quiescence needs gone/reused.
                state = "zombie" if fields[0] in ("Z", "X") else "running"
                if identity(injector["pid"]) != observed:
                    state = "reused"
        except (FileNotFoundError, ProcessLookupError):
            state = "gone"
    return {
        "present": True,
        "phase": value["phase"],
        "target": target,
        "injector": injector,
        "injector_state": state,
        "injector_quiescent": state in ("gone", "reused"),
        "effect_confirmed": False,
    }


def api(path: str, method: str, body: dict) -> dict:
    members = ("db1", "db2", "db3")
    if not isinstance(body, dict):
        raise TypeError("Invalid fault API body")
    switch = (
        path == "/switchover"
        and method == "POST"
        and set(body) == {"leader", "candidate"}
        and body["leader"] in members
        and body["candidate"] in members
        and body["leader"] != body["candidate"]
    )
    count = (
        path == "/config"
        and method == "PATCH"
        and set(body) == {"synchronous_node_count"}
        and isinstance(body["synchronous_node_count"], int)
        and not isinstance(body["synchronous_node_count"], bool)
        and body["synchronous_node_count"] in (1, 2)
    )
    cluster = path == "/cluster" and method == "GET" and not body
    if not (switch or count or cluster):
        raise ValueError("Unsupported fault API operation")
    with (
        deadline(10),
        closing(
            http.client.HTTPConnection("127.0.0.1", 8008, timeout=10)
        ) as connection,
    ):
        connection.request(
            method,
            path,
            body=None if cluster else json.dumps(body).encode(),
            headers={"Content-Type": "application/json"},
        )
        response = connection.getresponse()
        data = response.read(65537)
        if len(data) > 65536 or response.status != 200:
            raise ValueError("Unexpected Patroni response")
        if cluster:
            value = json.loads(data)
            if not isinstance(value, dict) or not isinstance(
                value.get("members"), list
            ):
                raise ValueError("Invalid Patroni cluster response")
            return value
        if count:
            connection.request("GET", "/config")
            response = connection.getresponse()
            data = response.read(65537)
            if response.status != 200 or len(data) > 65536:
                raise ValueError("Effective Patroni config request failed")
            value = json.loads(data)
            if (
                not isinstance(value, dict)
                or not isinstance(value.get("synchronous_node_count"), int)
                or isinstance(value.get("synchronous_node_count"), bool)
                or value.get("synchronous_node_count") != body["synchronous_node_count"]
                or value.get("synchronous_mode") is not True
                or value.get("synchronous_mode_strict") is not True
            ):
                raise ValueError("Effective Patroni synchronous config differs")
            return {
                "status": 200,
                "operation": "sync-count",
                "effective_count": value["synchronous_node_count"],
                "synchronous_mode": True,
                "synchronous_mode_strict": True,
            }
        # Config responses can contain unrelated confidential fields; never emit them.
        return {
            "status": response.status,
            "operation": "switch" if switch else "sync-count",
        }


def sql(command: str) -> dict:
    if command not in SQL.values():
        raise ValueError("Unsupported fault SQL operation")
    import psycopg2

    with closing(
        psycopg2.connect(
            user="postgres", host="/run/postgresql", dbname="pdns", connect_timeout=2
        )
    ) as connection:
        connection.autocommit = True
        with connection.cursor() as cursor:
            cursor.execute("SET statement_timeout='3000ms'")
            cursor.execute(command)
            return {"rows": cursor.fetchall()}


def campaign(action: str, nonce: str) -> dict:
    if (
        action not in ("acquire", "release")
        or re.fullmatch(r"[0-9a-f]{32}", nonce) is None
    ):
        raise ValueError("Invalid campaign operation or nonce")
    lock = EVIDENCE / "campaign.lock"
    if action == "acquire":
        lock.mkdir(mode=0o700)
    directory = os.open(lock, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        info = os.fstat(directory)
        if stat.S_IMODE(info.st_mode) != 0o700 or info.st_uid != os.geteuid():
            raise ValueError("Invalid campaign lock identity")
        flags = os.O_NOFOLLOW | (
            os.O_WRONLY | os.O_CREAT | os.O_EXCL if action == "acquire" else os.O_RDONLY
        )
        descriptor = os.open("nonce", flags, 0o600, dir_fd=directory)
        with os.fdopen(descriptor, "wb" if action == "acquire" else "rb") as stream:
            if action == "acquire":
                stream.write(nonce.encode("ascii"))
                stream.flush()
                os.fsync(stream.fileno())
            else:
                info = os.fstat(stream.fileno())
                if (
                    not stat.S_ISREG(info.st_mode)
                    or stat.S_IMODE(info.st_mode) != 0o600
                    or info.st_nlink != 1
                    or info.st_uid != os.geteuid()
                    or stream.read(33) != nonce.encode("ascii")
                ):
                    raise ValueError("Campaign lock nonce or identity mismatch")
        if action == "release":
            os.unlink("nonce", dir_fd=directory)
        os.fsync(directory)
    finally:
        os.close(directory)
    if action == "release":
        lock.rmdir()
    return {"acquired" if action == "acquire" else "released": True}


def main(args: list[str]) -> object:  # noqa: PLR0911, PLR0912 # Explicit closed lab dispatcher.
    match args:
        case ["signal-token", nonce, pid, start, name] if name in (
            "STOP",
            "CONT",
            "KILL",
        ):
            return signal_token(nonce, int(pid), start, signal.Signals["SIG" + name])
        case ["signal-status", nonce]:
            return signal_status(nonce)
        case ["campaign-acquire", nonce]:
            return campaign("acquire", nonce)
        case ["campaign-release", nonce]:
            return campaign("release", nonce)
        case ["cluster"]:
            return api("/cluster", "GET", {})
        case ["signal", pid, start, name] if name in ("STOP", "CONT", "KILL"):
            return send(int(pid), start, signal.Signals["SIG" + name])
        case ["pg-stop", nonce]:
            return pg_stop(nonce)
        case ["pg-resume-token", nonce]:
            return pg_resume_token(nonce)
        case ["switch", leader, candidate] if leader in (
            "db1",
            "db2",
            "db3",
        ) and candidate in ("db1", "db2", "db3"):
            return api(
                "/switchover", "POST", {"leader": leader, "candidate": candidate}
            )
        case ["sync-count", count] if count in ("1", "2"):
            return api("/config", "PATCH", {"synchronous_node_count": int(count)})
        case ["replay", value] if value in ("pause", "resume"):
            return sql(SQL[value])
        case ["wal"]:
            return sql(SQL["wal"])
        case _:
            raise ValueError("Unknown closed lab fault operation")


if __name__ == "__main__":
    print(json.dumps(main(sys.argv[1:])))
