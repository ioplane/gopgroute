# /// script
# requires-python = ">=3.13"
# dependencies = ["psycopg2-binary==2.9.10"]
# ///
"""Read-only, container-local observations for the isolated full-stack fixture."""
# ruff: noqa: PLR2004, PLC0415, TRY003
# Fixture sizes/status codes stay literal; SQL-only dependencies are lazy;
# fixed boundary errors do not require one exception class per message.

from __future__ import annotations

import argparse
import errno
import hashlib
import http.client
import ipaddress
import json
import re
import signal
import socket
import ssl
import subprocess  # nosec B404 # Fixed local read-only commands, never a shell.
import sys
import time
from contextlib import closing
from datetime import UTC, datetime
from pathlib import Path
from typing import cast

LIMIT = 1024 * 1024
IPS = {1: "192.0.2.10", 2: "198.51.100.10", 3: "203.0.113.10"}
VIPS = {"write": "198.19.255.10/32", "read": "198.19.255.11/32"}


class ProbeError(Exception):
    """A missing or malformed observation must stop its caller."""


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ProbeError(message)


def field[T](value: object, name: str, kind: type[T]) -> T:
    if not isinstance(value, dict) or name not in value:
        raise ProbeError("missing field: " + name)
    result = value[name]
    require(
        isinstance(result, kind) and not (kind is int and isinstance(result, bool)),
        "invalid field: " + name,
    )
    return result


def decode(data: bytes) -> object:
    require(len(data) <= LIMIT, "response exceeds limit")

    def pairs(items: list[tuple[str, object]]) -> dict:
        require(len(dict(items)) == len(items), "duplicate JSON field")
        return dict(items)

    def constant(_value: str) -> object:
        raise ProbeError("non-finite JSON number")

    return json.loads(data, object_pairs_hook=pairs, parse_constant=constant)


def read(path: Path) -> bytes:
    with path.open("rb") as stream:
        data = stream.read(LIMIT + 1)
    require(len(data) <= LIMIT, "file exceeds limit")
    return data


def digest(path: Path) -> str:
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def start_time(data: bytes, pid: int) -> str:
    text = data.decode()
    require(text.startswith(f"{pid} ("), "process stat PID mismatch")
    tail = text.rsplit(") ", 1)
    require(len(tail) == 2, "invalid process stat command")
    fields = tail[1].split()
    require(len(fields) >= 20 and fields[19].isdecimal(), "invalid process start time")
    return fields[19]


def process_start(process: Path) -> str:
    return start_time(read(process / "stat"), int(process.name))


def request(
    host: str,
    port: int,
    path: str,
    context: ssl.SSLContext | None = None,
    *,
    post: bool = False,
) -> tuple[int, object]:
    def expired(_signal: int, _frame: object) -> None:
        raise TimeoutError("HTTP deadline exceeded")

    previous = signal.signal(signal.SIGALRM, expired)
    signal.setitimer(signal.ITIMER_REAL, 2)
    try:
        connection = (
            http.client.HTTPConnection(host, port, timeout=2)
            if context is None
            else http.client.HTTPSConnection(host, port, timeout=2, context=context)
        )
        with closing(connection):
            connection.request(
                "POST" if post else "GET", path, body=b"{}" if post else None
            )
            response = connection.getresponse()
            return response.status, decode(response.read(LIMIT + 1))
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous)


def command(argv: list[str]) -> object:
    result = subprocess.run(argv, capture_output=True, check=True, timeout=2)  # noqa: S603 # Fixed read-only CLI argv from leaf().
    return decode(result.stdout)


def config() -> tuple[dict, int, str]:
    raw = read(Path("/state/gopgroute.json"))
    value = decode(raw)
    index = next(
        (i for i, address in IPS.items() if field(value, "next_hop", str) == address),
        None,
    )
    require(index is not None, "configuration outside fixture")
    for name, expected in (
        ("runtime_dir", "/run/gopgroute"),
        ("speaker_binary", "/usr/local/bin/gobgpd"),
        *VIPS.items(),
    ):
        require(field(value, name, str) == expected, "unexpected fixture configuration")
    require(
        field(value, "journal_dir", str) in ("/state/routes", "/state/routes-lease-v1"),
        "unexpected fixture journal root",
    )
    return cast(dict, value), cast(int, index), hashlib.sha256(raw).hexdigest()


def patroni(status: object, cluster: object, codes: dict[str, int]) -> dict:
    members = field(cluster, "members", list)
    require(len(members) == 3, "expected three Patroni members")
    projected = []
    for member in members:
        projected.append(
            {name: field(member, name, str) for name in ("name", "role", "state")}
        )
        lag = member.get("lag")
        require(
            lag is None
            or (isinstance(lag, int) and not isinstance(lag, bool) and lag >= 0)
            or lag == "unknown",
            "invalid Patroni lag",
        )
        projected[-1]["lag"] = lag
    require(
        len({member["name"] for member in projected}) == 3, "duplicate Patroni member"
    )
    return {
        "state": field(status, "state", str),
        "role": field(status, "role", str),
        "postgresql_start_time": field(status, "postmaster_start_time", str),
        "http_codes": codes,
        "members": projected,
    }


def etcd_members(value: object) -> list[dict]:
    rows = field(value, "members", list)
    require(len(rows) == 3, "expected three etcd members")
    result = []
    for row in rows:
        identifier = field(row, "ID", str)
        require(
            identifier.isdecimal() and int(identifier) > 0, "invalid etcd member ID"
        )
        learner = row.get("isLearner", False)  # Proto3 JSON omits the false default.
        require(isinstance(learner, bool), "invalid etcd learner flag")
        result.append(
            {
                "id": identifier,
                "name": field(row, "name", str),
                "is_learner": learner,
                "peer_urls": field(row, "peerURLs", list),
                "client_urls": field(row, "clientURLs", list),
            }
        )
        for key in ("peer_urls", "client_urls"):
            require(
                all(
                    isinstance(url, str) and url.startswith("https://")
                    for url in result[-1][key]
                ),
                "invalid etcd URL",
            )
    require(
        len({row["id"] for row in result}) == 3
        and len({row["name"] for row in result}) == 3,
        "duplicate etcd member",
    )
    return result


def postgres() -> dict:
    import psycopg2

    with (
        closing(
            psycopg2.connect(
                host="/run/postgresql",
                user="postgres",
                dbname="postgres",
                connect_timeout=2,
                options="-c statement_timeout=1500 -c default_transaction_read_only=on",
            )
        ) as connection,
        connection,
        connection.cursor() as cursor,
    ):
        cursor.execute(
            """SELECT json_build_object(
            'recovery',pg_is_in_recovery(),
            'postmaster_started',pg_postmaster_start_time()::text,
            'synchronous_commit',current_setting('synchronous_commit'),
            'wal_sender_timeout_ms',(SELECT setting::int FROM pg_settings
              WHERE name='wal_sender_timeout'),
            'wal_receiver_timeout_ms',(SELECT setting::int FROM pg_settings
              WHERE name='wal_receiver_timeout'),
            'synchronous_standby_names',current_setting('synchronous_standby_names'),
            'replication',(SELECT coalesce(json_agg(x),'[]') FROM (
              SELECT r.pid,r.application_name,r.state,r.sync_state,
              pg_wal_lsn_diff(CASE WHEN pg_is_in_recovery()
                THEN pg_last_wal_replay_lsn() ELSE pg_current_wal_lsn() END,
                r.replay_lsn)::bigint AS replay_lag_bytes,
              s.ssl,s.version AS ssl_version
              FROM pg_stat_replication r LEFT JOIN pg_stat_ssl s USING(pid)) x),
            'wal_receiver',(SELECT coalesce(json_agg(x),'[]') FROM (
              SELECT status,sender_host,sender_port,slot_name
              FROM pg_stat_wal_receiver) x),
            'slots',(SELECT coalesce(json_agg(x),'[]') FROM (
              SELECT slot_name,slot_type,active,
              pg_wal_lsn_diff(CASE WHEN pg_is_in_recovery()
                THEN pg_last_wal_replay_lsn() ELSE pg_current_wal_lsn() END,
                restart_lsn)::bigint AS lag_bytes FROM pg_replication_slots) x))"""
        )
        row = cursor.fetchone()
    require(row is not None, "missing PostgreSQL observation")
    value = row[0]
    for name, kind in (
        ("recovery", bool),
        ("postmaster_started", str),
        ("synchronous_commit", str),
        ("wal_sender_timeout_ms", int),
        ("wal_receiver_timeout_ms", int),
        ("synchronous_standby_names", str),
        ("replication", list),
        ("wal_receiver", list),
        ("slots", list),
    ):
        field(value, name, kind)
    return value


def node() -> dict:
    _, index, _ = config()
    codes, bodies = {}, {}
    for label, path in (
        ("patroni", "/patroni"),
        ("primary", "/primary"),
        ("read_write", "/read-write"),
        ("replica", "/replica?lag=1048576"),
        ("cluster", "/cluster"),
    ):
        codes[label], bodies[label] = request("127.0.0.1", 8008, path)
        require(
            codes[label] in ((200,) if label in ("patroni", "cluster") else (200, 503)),
            "unexpected Patroni HTTP status",
        )
    context = ssl.create_default_context(cafile="/run/lab/ca.crt")
    context.load_cert_chain(f"/run/lab/db{index}.crt", f"/run/lab/db{index}.key")
    code, health = request(IPS[index], 2379, "/health", context)
    require(
        code == 200 and field(health, "health", str) == "true", "etcd is not healthy"
    )
    code, members = request(
        IPS[index], 2379, "/v3/cluster/member/list", context, post=True
    )
    require(code == 200, "etcd member request failed")
    pids = {
        name: int(read(path).splitlines()[0])
        for name, path in (
            ("patroni", Path("/run/patroni.pid")),
            ("etcd", Path("/run/etcd.pid")),
            ("pgbouncer", Path("/run/pgbouncer.pid")),
            ("postgres", Path("/state/pg/postmaster.pid")),
        )
    }
    require(
        all(pid > 0 and Path(f"/proc/{pid}").exists() for pid in pids.values()),
        "missing node process",
    )
    pg = postgres()
    pg["postgres_pid"] = pids["postgres"]
    return {
        "node_index": index,
        "process_ids": pids,
        "process_start_ids": {
            role: process_start(Path(f"/proc/{pid}")) for role, pid in pids.items()
        },
        "postgres": pg,
        "patroni": patroni(bodies["patroni"], bodies["cluster"], codes),
        "etcd": {"health": True, "members": etcd_members(members)},
    }


def classify_processes(rows: list[dict], guardian_hash: str, speaker_hash: str) -> dict:
    require(len(rows) == 3, "expected exactly three lease holders")
    guardian = [
        row
        for row in rows
        if row["exe_sha256"] == guardian_hash
        and not any(parent["pid"] == row["ppid"] for parent in rows)
    ]
    require(len(guardian) == 1, "ambiguous guardian identity")
    children = [row for row in rows if row["ppid"] == guardian[0]["pid"]]
    adapters = [
        row
        for row in children
        if row["exe_sha256"] == guardian_hash
        and {item["fd"] for item in row["leases"]} == {4, 5}
    ]
    speakers = [
        row
        for row in children
        if row["exe_sha256"] == speaker_hash
        and {item["fd"] for item in row["leases"]} == {3, 4}
    ]
    require(len(adapters) == len(speakers) == 1, "ambiguous supervised child identity")
    return {"guardian": guardian[0], "adapter": adapters[0], "speaker": speakers[0]}


def active_generation(adapter: Path, root: Path) -> str:
    active = []
    for fd in (adapter / "fd").iterdir():
        target = str(fd.readlink())
        match = re.fullmatch(re.escape(str(root)) + r"/(i-[0-9a-f]{32})/lock", target)
        if match is not None:
            require(fd.samefile(Path(target)), "active generation lock changed")
            active.append(match[1])
    require(len(active) == 1, "ambiguous active generation")
    return active[0]


def route() -> dict:
    cfg, _, cfg_hash = config()
    roots = [Path(cfg[key]) for key in ("runtime_dir", "journal_dir")]
    identities = {}
    for root in roots:
        info = (root / "guardian.lock").stat()
        identities[(info.st_dev, info.st_ino)] = root
    rows = []
    for process in Path("/proc").iterdir():
        if not process.name.isdecimal():
            continue
        leases = []
        try:
            started = process_start(process)
            for fd in (process / "fd").iterdir():
                info = fd.stat()
                if (info.st_dev, info.st_ino) in identities:
                    root = identities[(info.st_dev, info.st_ino)]
                    lock = read(process / "fdinfo" / fd.name).decode()
                    require(
                        re.search(r"lock:\s+\d+: FLOCK\s+ADVISORY\s+WRITE\s", lock)
                        is not None,
                        "lease not write-locked",
                    )
                    leases.append(
                        {
                            "root": str(root),
                            "fd": int(fd.name),
                            "device": info.st_dev,
                            "inode": info.st_ino,
                            "flock_write": True,
                        }
                    )
            if leases:
                require(
                    len(leases) == 2 and len({item["root"] for item in leases}) == 2,
                    "partial incarnation lease holder",
                )
                status = read(process / "status").decode()
                ppid = re.search(r"^PPid:\s+(\d+)$", status, re.MULTILINE)
                if ppid is None:
                    raise ProbeError("missing process parent")
                rows.append(
                    {
                        "pid": int(process.name),
                        "ppid": int(ppid[1]),
                        "start_time": started,
                        "exe_sha256": digest(process / "exe"),
                        "leases": leases,
                    }
                )
                require(
                    process_start(process) == started,
                    "process changed during observation",
                )
        except FileNotFoundError:
            require(
                not leases, "lease holder changed during observation"
            )  # fail-silent-ok: unrelated /proc entries can vanish during enumeration.
    guardian_hash, speaker_hash = (
        digest(Path("/usr/local/bin/gopgroute")),
        digest(Path(cfg["speaker_binary"])),
    )
    require(speaker_hash == cfg["speaker_sha256"], "speaker binary changed")
    generations = []
    for generation in sorted(roots[1].iterdir()):
        if generation.name == "guardian.lock":
            continue
        require(
            re.fullmatch(r"i-[0-9a-f]{32}", generation.name) is not None
            and generation.is_dir()
            and not generation.is_symlink(),
            "unknown generation entry",
        )
        journal, staging = generation / "journal.json", generation / "journal.next"
        entry = {
            "name": generation.name,
            "journal_sha256": None,
            "staging_sha256": digest(staging) if staging.exists() else None,
            "phases": [],
            "uuid_sha256s": [],
        }
        if journal.exists():
            journal_bytes = read(journal)
            value = decode(journal_bytes)
            require(
                field(value, "Version", int) == 1
                and field(value, "NextHop", str) == cfg["next_hop"]
                and field(value, "Write", str) == cfg["write"]
                and field(value, "Read", str) == cfg["read"],
                "journal identity mismatch",
            )
            entries = field(value, "Entries", list)
            require(len(entries) == 2, "invalid journal entries")
            for item in entries:
                phase, uuid = field(item, "Phase", str), field(item, "UUID", str)
                require(
                    phase in ("", "pending-add", "owned", "pending-delete"),
                    "invalid journal phase",
                )
                require(
                    (
                        uuid == ""
                        if phase in ("", "pending-add")
                        else re.fullmatch(r"[0-9a-f]{32}", uuid) is not None
                        and int(uuid, 16) != 0
                    ),
                    "invalid journal UUID",
                )
                entry["phases"].append(phase)
                entry["uuid_sha256s"].append(
                    hashlib.sha256(bytes.fromhex(uuid)).hexdigest() if uuid else None
                )
            entry["journal_sha256"] = hashlib.sha256(journal_bytes).hexdigest()
        generations.append(entry)
    processes = classify_processes(rows, guardian_hash, speaker_hash)
    adapter = Path(f"/proc/{processes['adapter']['pid']}")
    return {
        "config_sha256": cfg_hash,
        "pid_namespace": str(Path("/proc/self/ns/pid").stat().st_ino),
        "binary_sha256": {"guardian": guardian_hash, "speaker": speaker_hash},
        "processes": processes,
        "active_generation": active_generation(adapter, roots[1]),
        "generations": generations,
    }


def peers(value: object) -> list[dict]:
    if not isinstance(value, dict) or len(value) != 3:
        raise ProbeError("expected three leaf peers")
    result = []
    for address, peer in value.items():
        gr = field(peer, "gracefulRestartInfo", dict)
        family = field(field(peer, "addressFamilyInfo", dict), "ipv4Unicast", dict)
        accepted = field(family, "acceptedPrefixCounter", int)
        require(accepted >= 0, "invalid accepted prefix count")
        result.append(
            {
                "address": address,
                "state": field(peer, "bgpState", str),
                "local_gr_mode": field(gr, "localGrMode", str),
                "accepted_prefixes": accepted,
                "remote_gr_mode": gr.get("remoteGrMode"),
            }
        )
        require(
            result[-1]["remote_gr_mode"] is None
            or isinstance(result[-1]["remote_gr_mode"], str),
            "invalid remote GR mode",
        )
    return result


def fib(value: object, prefix: str, kernel: object) -> dict:
    if not isinstance(value, dict) or not isinstance(kernel, list):
        raise ProbeError("invalid route response")
    require(set(value).issubset({prefix}), "unexpected route prefix")
    routes = value.get(
        prefix, []
    )  # An empty successful RIB response means observed withdrawal.
    require(isinstance(routes, list), "invalid route entries")
    installed, hops, active_hops = [], [], set()
    for row in routes:
        installed.append(field(row, "installed", bool))
        for hop in field(row, "nexthops", list):
            projected = {
                "gateway": field(hop, "ip", str),
                "fib": field(hop, "fib", bool),
            }
            hops.append(projected)
            if installed[-1] and projected["fib"]:
                active_hops.add(str(ipaddress.IPv4Address(projected["gateway"])))
    kernel_hops = set()
    for row in kernel:
        require(
            str(ipaddress.ip_network(field(row, "dst", str))) == prefix,
            "unexpected kernel route",
        )
        require(
            row.get("type", "unicast") == "unicast", "kernel VIP route is not unicast"
        )
        if "nexthops" in row:
            entries = field(row, "nexthops", list)
            require(bool(entries), "empty kernel nexthop list")
        else:
            entries = [row]
        for hop in entries:
            kernel_hops.add(str(ipaddress.IPv4Address(field(hop, "gateway", str))))
            require(bool(field(hop, "dev", str)), "missing kernel nexthop device")
    rib_installed, kernel_installed = any(installed), bool(kernel)
    require(rib_installed == kernel_installed, "RIB/kernel VIP installation mismatch")
    require(
        not rib_installed or (bool(active_hops) and active_hops == kernel_hops),
        "RIB/kernel VIP nexthop mismatch",
    )
    return {
        "prefix": prefix,
        "rib_installed": rib_installed,
        "kernel_installed": kernel_installed,
        "installed": rib_installed and kernel_installed,
        "nexthops": hops,
        "kernel_routes": kernel,
    }


def origin_prefixes(value: object, peer: str) -> list[str]:
    routes = field(value, "routes", dict)
    require(set(routes).issubset(set(VIPS.values())), "foreign local-origin prefix")
    require(
        field(value, "totalRoutes", int) == len(routes), "origin route count mismatch"
    )
    index = next((i for i, address in IPS.items() if address == peer), None)
    require(index is not None, "unknown local GoBGP peer")
    for prefix, paths in routes.items():
        require(isinstance(paths, list) and bool(paths), "invalid origin paths")
        for path in paths:
            require(
                field(path, "peerId", str) == peer and field(path, "valid", bool),
                "invalid origin peer or route",
            )
            require(
                field(path, "network", str) == prefix
                and field(path, "prefixLen", int) == 32,
                "origin network mismatch",
            )
            require(
                str(ipaddress.IPv4Address(field(path, "prefix", str)))
                == prefix.removesuffix("/32"),
                "origin prefix mismatch",
            )
            require(
                field(path, "path", str) == str(64520 + cast(int, index)),
                "origin AS path mismatch",
            )
            nexthops = field(path, "nexthops", list)
            require(bool(nexthops), "missing origin next hop")
            require(
                all(field(hop, "ip", str) == peer for hop in nexthops),
                "origin next hop differs from peer",
            )
    return sorted(routes)


def leaf() -> dict:
    neighbors = command(["/usr/bin/vtysh", "-c", "show bgp neighbors json"])
    peer_observations = peers(neighbors)
    local = [
        row["address"] for row in peer_observations if row["address"] in IPS.values()
    ]
    require(len(local) == 1, "ambiguous local GoBGP peer")
    origins = origin_prefixes(
        command(
            [
                "/usr/bin/vtysh",
                "-c",
                "show bgp ipv4 unicast neighbors " + local[0] + " routes json",
            ]
        ),
        local[0],
    )
    routes = {}
    for label, prefix in VIPS.items():
        routes[label] = fib(
            command(["/usr/bin/vtysh", "-c", "show ip route " + prefix + " json"]),
            prefix,
            command(["/usr/sbin/ip", "-j", "route", "show", "exact", prefix]),
        )
    return {
        "peers": peer_observations,
        "vips": routes,
        "local_origin_prefixes": origins,
    }


def client() -> dict:
    import psycopg2

    checks = {}
    for label, prefix in VIPS.items():
        try:
            with (
                closing(
                    psycopg2.connect(
                        host=prefix.split("/")[0],
                        port=6432,
                        dbname="pdns",
                        user="pdns",
                        password=Path("/run/lab/password")
                        .read_text(encoding="utf-8")
                        .strip(),
                        sslmode="verify-full",
                        sslrootcert="/run/lab/ca.crt",
                        connect_timeout=2,
                    )
                ) as connection,
                connection,
                connection.cursor() as cursor,
            ):
                cursor.execute("SET LOCAL statement_timeout='1500ms'")
                cursor.execute(
                    "SELECT pg_is_in_recovery(),pg_postmaster_start_time()::text"
                )
                row = cursor.fetchone()
                require(
                    row is not None and isinstance(row[0], bool),
                    "invalid SQL role result",
                )
                checks[label] = {
                    "ok": row[0] == (label == "read"),
                    "recovery": row[0],
                    "postmaster_started": row[1],
                    "error": None,
                }
        except (psycopg2.Error, OSError, ProbeError) as error:
            checks[label] = {"ok": False, "error": type(error).__name__}
    try:
        answer = subprocess.run(
            [
                "/usr/bin/dig",
                "@198.19.0.10",
                "-p",
                "5300",
                "probe.ha.example.test",
                "A",
                "+short",
                "+time=1",
                "+tries=1",
            ],
            capture_output=True,
            text=True,
            check=True,
            timeout=2,
        ).stdout.strip()
        checks["dns"] = {"ok": answer == "203.0.113.42", "error": None}
    except (subprocess.SubprocessError, OSError) as error:
        checks["dns"] = {"ok": False, "error": type(error).__name__}
    return {"ok": all(item["ok"] for item in checks.values()), "checks": checks}


def ledger() -> dict:
    import psycopg2

    with (
        closing(
            psycopg2.connect(
                host="198.19.255.10",
                port=6432,
                dbname="pdns",
                user="pdns",
                password=Path("/run/lab/password").read_text(encoding="utf-8").strip(),
                sslmode="verify-full",
                sslrootcert="/run/lab/ca.crt",
                connect_timeout=2,
            )
        ) as connection,
        connection,
        connection.cursor() as cursor,
    ):
        cursor.execute("SET LOCAL statement_timeout='1500ms'")
        cursor.execute("SET TRANSACTION READ ONLY")
        cursor.execute("SELECT id,payload FROM lab_receipts ORDER BY id LIMIT 10001")
        rows = cursor.fetchall()
    require(len(rows) <= 10000, "ledger response exceeds limit")
    require(
        all(
            isinstance(identifier, int)
            and not isinstance(identifier, bool)
            and isinstance(payload, str)
            for identifier, payload in rows
        ),
        "invalid ledger rows",
    )
    return {"rows": rows}


def fencing_sql() -> dict:
    import psycopg2

    try:
        with socket.create_connection(("127.0.0.1", 5432), timeout=2):
            pass  # A TCP listener alone cannot authorize a PostgreSQL role.
    except OSError as error:
        refused = (
            isinstance(error, ConnectionRefusedError)
            and error.errno == errno.ECONNREFUSED
        )
        return {
            "state": "refused" if refused else "unknown",
            "sql_accepts": False if refused else None,
            "error": type(error).__name__,
            "backend": "127.0.0.1:5432",
            "refusal_errno": errno.ECONNREFUSED if refused else None,
        }
    try:
        with (
            closing(
                psycopg2.connect(
                    host="/run/postgresql",
                    user="postgres",
                    dbname="postgres",
                    connect_timeout=2,
                    options=(
                        "-c statement_timeout=1500 -c default_transaction_read_only=on"
                    ),
                )
            ) as connection,
            connection,
            connection.cursor() as cursor,
        ):
            cursor.execute("SELECT pg_is_in_recovery()")
            row = cursor.fetchone()
    except (psycopg2.Error, OSError) as error:
        return {"state": "unknown", "sql_accepts": None, "error": type(error).__name__}
    if (
        not isinstance(row, (tuple, list))
        or len(row) != 1
        or not isinstance(row[0], bool)
    ):
        return {"state": "unknown", "sql_accepts": None, "error": "InvalidSQLResult"}
    return {
        "state": "demoted" if row[0] else "primary",
        "sql_accepts": True,
        "recovery": row[0],
        "error": None,
        "criterion": "Read-only SQL accepted; acknowledged writes were not tested.",
    }


def bounded_fencing(argv: list[str], timeout: float = 6) -> dict:
    """Worker completion and database write exclusion remain separate facts."""
    child = subprocess.Popen(  # noqa: S603 # nosec B603 # Fixed read-only worker; tests supply an isolated child.
        argv,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
    )
    identity: dict[str, object] = {
        "worker_pid": child.pid,
        "worker_start_time": None,
        "pid_namespace": None,
    }
    try:
        identity["worker_start_time"] = process_start(Path(f"/proc/{child.pid}"))
        identity["pid_namespace"] = str(Path("/proc/self/ns/pid").stat().st_ino)
        output, _ = child.communicate(timeout=timeout)
    except BaseException as error:  # noqa: BLE001 # Every post-spawn failure retains ownership through bounded cleanup.
        failure = (
            "WorkerTimeout"
            if isinstance(error, subprocess.TimeoutExpired)
            else type(error).__name__
        )
        try:
            child.kill()
        except OSError as kill_error:
            identity["kill_error"] = type(kill_error).__name__
        try:
            child.communicate(timeout=1)
        except BaseException:  # noqa: BLE001 # Even interrupted cleanup must retain an unconfirmed worker.
            return {
                **identity,
                "state": "unknown",
                "sql_accepts": None,
                "execution_stopped": False,
                "error": "WorkerStopUnconfirmed",
            }
        return {
            **identity,
            "state": "unknown",
            "sql_accepts": None,
            "execution_stopped": True,
            "error": failure,
        }
    if child.returncode != 0:
        return {
            **identity,
            "state": "unknown",
            "sql_accepts": None,
            "execution_stopped": True,
            "error": "WorkerFailed",
        }
    value = decode(output)
    require(isinstance(value, dict), "invalid fencing worker response")
    value = cast(dict, value)
    require(
        field(value, "state", str) in ("primary", "demoted", "refused", "unknown"),
        "invalid fencing worker state",
    )
    require(
        "sql_accepts" in value
        and value["sql_accepts"]
        is {"primary": True, "demoted": True, "refused": False, "unknown": None}[
            value["state"]
        ],
        "inconsistent fencing worker SQL observation",
    )
    return {**value, **identity, "execution_stopped": True}


def fencing() -> dict:
    value = bounded_fencing(
        [sys.executable, str(Path(__file__).resolve()), "fencing-sql"]
    )
    value["mode"] = "fencing"
    return value


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    modes = {
        "node": node,
        "route": route,
        "leaf": leaf,
        "client": client,
        "ledger": ledger,
        "fencing": fencing,
        "fencing-sql": fencing_sql,
    }
    parser.add_argument("mode", choices=modes)
    args = parser.parse_args()
    started = time.monotonic()
    value = {
        "schema_version": 1,
        "mode": args.mode,
        "observed_at": datetime.now(UTC).isoformat(),
        **modes[args.mode](),
    }
    value["elapsed_seconds"] = time.monotonic() - started
    print(json.dumps(value, allow_nan=False))


if __name__ == "__main__":
    try:
        main()
    except Exception as error:  # noqa: BLE001 # CLI boundary emits a safe type and exits nonzero without raw SQL/config/credentials.
        print(
            json.dumps(
                {
                    "error": type(error).__name__,
                    "detail": str(error) if isinstance(error, ProbeError) else None,
                }
            ),
            file=sys.stderr,
        )
        raise SystemExit(1) from None
