# /// script
# requires-python = ">=3.13"
# dependencies = ["psycopg2-binary==2.9.10", "PyYAML==6.0.2"]
# ///
"""Isolated compose processes; never changes the host network namespace."""

from __future__ import annotations

import hashlib
import json
import os
import pwd
import secrets
import signal
import subprocess  # nosec B404 # Fixed local executables, no shell or external argv.
import sys
import time
from pathlib import Path

IPS = {1: "192.0.2.10", 2: "198.51.100.10", 3: "203.0.113.10"}
NETS = {1: "192.0.2.0/26", 2: "198.51.100.0/26", 3: "203.0.113.0/26"}
WRITE = "198.19.255.10"
READ = "198.19.255.11"


def run(*args: str) -> None:
    subprocess.run(args, check=True, timeout=30)  # nosec B603 # Closed role command lists.


def private(path: str, content: str) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(content, encoding="utf-8")
    target.chmod(0o600)


def supervise(commands: list[tuple[list[str], str | None]]) -> None:
    children: list[subprocess.Popen[bytes]] = []

    def stop(_signum: int, _frame: object) -> None:
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    try:
        for argv, user in commands:
            environment = dict(os.environ)
            if user is not None:
                environment.update(
                    HOME=pwd.getpwnam(user).pw_dir, USER=user, LOGNAME=user
                )
            child = subprocess.Popen(argv, user=user, group=user, env=environment)  # nosec B603 # Closed role command lists.
            children.append(child)
            private(f"/run/{Path(argv[0]).name}.pid", str(child.pid))
        while True:
            for child in children:
                if child.poll() is not None:
                    raise RuntimeError(
                        f"Lab child {child.args!r} exited: {child.returncode}"
                    )
            time.sleep(0.2)
    finally:
        for child in children:
            if child.poll() is None:
                child.terminate()
        for child in children:
            try:
                child.wait(timeout=15)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait()


def bootstrap() -> None:
    import psycopg2
    from psycopg2 import sql

    password = Path("/run/lab/password").read_text().strip()
    connection = psycopg2.connect(user="postgres", host="/run/postgresql")
    try:
        connection.autocommit = True
        with connection.cursor() as cursor:
            cursor.execute("CREATE ROLE pdns_owner NOLOGIN")
            for name in ("pdns", "route_health"):
                cursor.execute(
                    sql.SQL("CREATE ROLE {} LOGIN PASSWORD %s").format(
                        sql.Identifier(name)
                    ),
                    (password,),
                )
            cursor.execute("CREATE DATABASE pdns OWNER pdns_owner")
    finally:
        connection.close()
    with (
        psycopg2.connect(
            user="postgres", host="/run/postgresql", dbname="pdns"
        ) as connection,
        connection.cursor() as cursor,
    ):
        schemas = list(
            Path("/usr/share").glob("pdns-backend-pgsql/**/schema.pgsql.sql")
        )
        if len(schemas) != 1:
            raise RuntimeError(f"Expected one packaged PowerDNS schema, got {schemas}")
        cursor.execute(schemas[0].read_text())
        cursor.execute("GRANT CONNECT ON DATABASE pdns TO pdns, route_health")
        cursor.execute("GRANT USAGE ON SCHEMA public TO pdns")
        cursor.execute(
            "GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO pdns"
        )
        cursor.execute("GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO pdns")
        cursor.execute(
            "INSERT INTO domains(name,type) VALUES ('ha.example.test','NATIVE') RETURNING id"
        )
        domain_id = cursor.fetchone()[0]
        for name, kind, value in (
            (
                "ha.example.test",
                "SOA",
                "ns.ha.example.test hostmaster.ha.example.test 1 60 30 3600 1",
            ),
            ("ha.example.test", "NS", "ns.ha.example.test"),
            ("ns.ha.example.test", "A", "198.19.0.10"),
            ("probe.ha.example.test", "A", "203.0.113.42"),
        ):
            cursor.execute(
                "INSERT INTO records(domain_id,name,type,content,ttl) VALUES (%s,%s,%s,%s,1)",
                (domain_id, name, kind, value),
            )
        cursor.execute(
            "CREATE TABLE lab_receipts(id bigint PRIMARY KEY, payload text NOT NULL)"
        )
        cursor.execute("GRANT SELECT, INSERT ON lab_receipts TO pdns")


def database(index: int) -> None:
    import yaml

    address = IPS[index]
    gateway = address.rsplit(".", 1)[0] + ".2"
    run("/usr/sbin/ip", "route", "replace", "default", "via", gateway)
    for vip in (WRITE, READ):
        run("/usr/sbin/ip", "address", "replace", f"{vip}/32", "dev", "lo")
    account = pwd.getpwnam("postgres")
    Path("/state").mkdir(mode=0o755, exist_ok=True)
    os.chown("/state", 0, 0)
    Path("/state").chmod(0o755)
    for directory in ("/state/pg", "/run/postgresql"):
        Path(directory).mkdir(parents=True, exist_ok=True)
        os.chown(directory, account.pw_uid, account.pw_gid)
        Path(directory).chmod(0o700)
    for directory in ("/state/runtime", "/state/routes"):
        Path(directory).mkdir(mode=0o700, parents=True, exist_ok=True)
    password = Path("/run/lab/password").read_text().strip()
    private("/state/route-password", password)
    cert = f"/run/lab/db{index}"
    patroni = {
        "scope": "gopgroute-lab",
        "name": f"db{index}",
        "restapi": {"listen": "0.0.0.0:8008", "connect_address": f"{address}:8008"},
        "etcd3": {
            "hosts": [f"{ip}:2379" for ip in IPS.values()],
            "protocol": "https",
            "cacert": "/run/lab/ca.crt",
            "cert": cert + ".crt",
            "key": cert + ".key",
        },
        "bootstrap": {
            "dcs": {
                "ttl": 20,
                "loop_wait": 3,
                "retry_timeout": 3,
                "synchronous_mode": True,
                "synchronous_mode_strict": True,
                "postgresql": {
                    "use_pg_rewind": True,
                    "parameters": {
                        "wal_level": "replica",
                        "hot_standby": "on",
                        "max_replication_slots": 10,
                        "max_wal_senders": 10,
                        "wal_sender_timeout": "10s",
                        "wal_receiver_timeout": "10s",
                        "max_slot_wal_keep_size": "256MB",
                        "password_encryption": "scram-sha-256",  # nosec B105 # Algorithm identifier.
                        "ssl": "on",
                        "ssl_ca_file": "/run/lab/ca.crt",
                        "ssl_cert_file": cert + ".crt",
                        "ssl_key_file": cert + ".key",
                        "shared_buffers": "64MB",
                        "max_connections": 40,
                    },
                },
            },
            "initdb": [
                "data-checksums",
                {"auth-local": "trust"},
                {"auth-host": "scram-sha-256"},
            ],
            "post_bootstrap": "/usr/bin/python3 /lab/node.py bootstrap",
        },
        "postgresql": {
            "listen": "0.0.0.0:5432",
            "basebackup": {"checkpoint": "fast"},
            "connect_address": f"{address}:5432",
            "bin_dir": "/usr/lib/postgresql/18/bin",
            "data_dir": "/state/pg",
            "authentication": {
                "superuser": {"username": "postgres", "password": password},
                "replication": {
                    "username": "replicator",
                    "password": password,
                    "sslmode": "verify-ca",
                    "sslrootcert": "/run/lab/ca.crt",
                },
            },
            "pg_hba": [
                "local all postgres trust",
                "host all all 127.0.0.1/32 scram-sha-256",
                "hostssl replication replicator 127.0.0.1/32 scram-sha-256",
            ]
            + [
                f"hostssl replication replicator {network} scram-sha-256"
                for network in NETS.values()
            ]
            + [
                f"hostssl all postgres {network} scram-sha-256"
                for network in NETS.values()
            ],
        },
        "watchdog": {"mode": "off"},
    }
    private("/run/patroni.yml", yaml.safe_dump(patroni))
    os.chown("/run/patroni.yml", account.pw_uid, account.pw_gid)
    private(
        "/run/pgbouncer-users", f'"pdns" "{password}"\n"route_health" "{password}"\n'
    )
    os.chown("/run/pgbouncer-users", account.pw_uid, account.pw_gid)
    pooler = f"[databases]\npdns = host=127.0.0.1 port=5432 dbname=pdns\n[pgbouncer]\nlisten_addr = 0.0.0.0\nlisten_port = 6432\nauth_type = scram-sha-256\nauth_file = /run/pgbouncer-users\npool_mode = transaction\nmax_client_conn = 40\ndefault_pool_size = 5\npidfile = /run/postgresql/pgbouncer.pid\nclient_tls_sslmode = allow\nclient_tls_cert_file = {cert}.crt\nclient_tls_key_file = {cert}.key\nclient_tls_ca_file = /run/lab/ca.crt\n"
    private("/run/pgbouncer.ini", pooler)
    os.chown("/run/pgbouncer.ini", account.pw_uid, account.pw_gid)
    settings = {
        "speaker_binary": "/usr/local/bin/gobgpd",
        "speaker_sha256": hashlib.sha256(
            Path("/usr/local/bin/gobgpd").read_bytes()
        ).hexdigest(),
        "runtime_dir": "/run/gopgroute",
        "journal_dir": "/state/routes",
        "write": WRITE + "/32",
        "read": READ + "/32",
        "next_hop": address,
        "asn": 64520 + index,
        "router_id": address,
        "peers": [{"address": gateway, "asn": 64510 + index, "port": 179}],
        "patroni_url": "http://127.0.0.1:8008",
        "max_lag_bytes": 1048576,
        "pooler_address": "127.0.0.1:6432",
        "backend": "127.0.0.1:5432",
        "database": "pdns",
        "user": "route_health",
        "password_file": "/state/route-password",  # nosec B105 # Private file path.
        "probe_timeout": "500ms",
        "rpc_timeout": "500ms",
        "max_age": "5s",
        "interval": "1s",
        "cycle_timeout": "3s",
        "heartbeat_timeout": "10s",
        "stop_timeout": "1s",
        "startup_timeout": "3s",
    }
    if not Path("/state/gopgroute.json").exists():
        private("/state/gopgroute.json", json.dumps(settings))
    cluster = ",".join(f"db{i}=https://{ip}:2380" for i, ip in IPS.items())
    etcd = [
        "/usr/bin/etcd",
        "--name",
        f"db{index}",
        "--data-dir",
        "/state/etcd",
        "--listen-client-urls",
        f"https://{address}:2379",
        "--advertise-client-urls",
        f"https://{address}:2379",
        "--listen-peer-urls",
        f"https://{address}:2380",
        "--initial-advertise-peer-urls",
        f"https://{address}:2380",
        "--initial-cluster",
        cluster,
        "--initial-cluster-token",
        "gopgroute-isolated",
        "--cert-file",
        cert + ".crt",
        "--key-file",
        cert + ".key",
        "--trusted-ca-file",
        "/run/lab/ca.crt",
        "--client-cert-auth",
        "--peer-cert-file",
        cert + ".crt",
        "--peer-key-file",
        cert + ".key",
        "--peer-trusted-ca-file",
        "/run/lab/ca.crt",
        "--peer-client-cert-auth",
    ]
    supervise(
        [
            (etcd, None),
            (["/usr/bin/patroni", "/run/patroni.yml"], "postgres"),
            (["/usr/sbin/pgbouncer", "/run/pgbouncer.ini"], "postgres"),
        ]
    )


def leaf(index: int) -> None:
    lines = [
        "frr defaults datacenter",
        f"hostname leaf{index}",
        "log stdout",
        f"router bgp {64510 + index}",
        f" bgp router-id 198.18.0.{10 + index}",
        " no bgp ebgp-requires-policy",
        " no bgp graceful-restart",
        f" neighbor {IPS[index]} remote-as {64520 + index}",
        f" neighbor {IPS[index]} graceful-restart-disable",
        f" neighbor {IPS[index]} timers 1 3",
    ]
    for peer in range(1, 4):
        if peer != index:
            lines += [
                f" neighbor 198.18.0.{10 + peer} remote-as {64510 + peer}",
                f" neighbor 198.18.0.{10 + peer} graceful-restart-disable",
                f" neighbor 198.18.0.{10 + peer} timers 1 3",
            ]
    lines += [" address-family ipv4 unicast", f"  network {NETS[index]}"]
    if index == 1:
        lines += ["  network 198.19.0.0/24"]
    lines += [" exit-address-family"]
    Path("/etc/frr/frr.conf").write_text("\n".join(lines) + "\n")
    Path("/run/frr").mkdir(exist_ok=True)
    account = pwd.getpwnam("frr")
    os.chown("/run/frr", account.pw_uid, account.pw_gid)
    supervise(
        [
            (["/usr/lib/frr/mgmtd", "-A", "127.0.0.1"], None),
            (
                ["/usr/lib/frr/zebra", "-f", "/etc/frr/frr.conf", "-A", "127.0.0.1"],
                None,
            ),
            (
                [
                    "/usr/lib/frr/bgpd",
                    "-f",
                    "/etc/frr/frr.conf",
                    "-A",
                    "127.0.0.1",
                ],
                None,
            ),
        ]
    )


def client() -> None:
    import psycopg2

    run("/usr/sbin/ip", "route", "replace", "default", "via", "198.19.0.2")
    password = Path("/run/lab/password").read_text().strip()
    deadline = time.monotonic() + 120
    while True:
        try:
            connection = psycopg2.connect(
                host=WRITE,
                port=6432,
                dbname="pdns",
                user="pdns",
                password=password,
                connect_timeout=1,
            )
        except psycopg2.OperationalError:
            if time.monotonic() > deadline:
                raise RuntimeError(
                    "Routed write service startup deadline exceeded"
                ) from None
            time.sleep(1)
        else:
            connection.close()
            break
    private(
        "/run/pdns.conf",
        f"launch=gpgsql\ngpgsql-host={WRITE}\ngpgsql-port=6432\ngpgsql-dbname=pdns\ngpgsql-user=pdns\ngpgsql-password={password}\nlocal-address=198.19.0.10\nlocal-port=5300\ncache-ttl=0\nquery-cache-ttl=0\nnegquery-cache-ttl=0\ndisable-syslog=yes\nloglevel=4\n",
    )
    supervise(
        [
            (
                [
                    "/usr/sbin/pdns_server",
                    "--config-dir=/run",
                    "--daemon=no",
                    "--guardian=no",
                ],
                None,
            )
        ]
    )


def credentials() -> None:
    directory = Path("/run/lab")
    if list(directory.iterdir()):
        raise RuntimeError("Credential directory must be empty")
    run(
        "/usr/bin/openssl",
        "req",
        "-x509",
        "-newkey",
        "rsa:2048",
        "-nodes",
        "-keyout",
        str(directory / "ca.key"),
        "-out",
        str(directory / "ca.crt"),
        "-days",
        "7",
        "-subj",
        "/CN=isolated-gopgroute-lab",
        "-addext",
        "keyUsage=critical,keyCertSign,cRLSign",
    )
    private(str(directory / "password"), secrets.token_urlsafe(32))
    account = pwd.getpwnam("postgres")
    for index, address in IPS.items():
        stem = directory / f"db{index}"
        ext = directory / f"db{index}.ext"
        ext.write_text(
            f"subjectAltName=IP:{address},IP:{WRITE},IP:{READ},IP:127.0.0.1\nextendedKeyUsage=serverAuth,clientAuth\n"
        )
        run(
            "/usr/bin/openssl",
            "req",
            "-new",
            "-newkey",
            "rsa:2048",
            "-nodes",
            "-keyout",
            str(stem) + ".key",
            "-out",
            str(stem) + ".csr",
            "-subj",
            f"/CN=db{index}",
        )
        run(
            "/usr/bin/openssl",
            "x509",
            "-req",
            "-in",
            str(stem) + ".csr",
            "-CA",
            str(directory / "ca.crt"),
            "-CAkey",
            str(directory / "ca.key"),
            "-CAcreateserial",
            "-out",
            str(stem) + ".crt",
            "-days",
            "7",
            "-extfile",
            str(ext),
        )
    for path in directory.iterdir():
        os.chown(path, 0, account.pw_gid)
        path.chmod(0o600 if path.name == "ca.key" else 0o640)
    os.chown(directory, 0, account.pw_gid)
    directory.chmod(0o750)


if __name__ == "__main__":
    mode = sys.argv[1]
    if mode == "bootstrap":
        bootstrap()
    elif mode == "credentials":
        credentials()
    elif mode == "db":
        database(int(sys.argv[2]))
    elif mode == "leaf":
        leaf(int(sys.argv[2]))
    elif mode == "client":
        client()
    elif mode == "route":
        deadline = time.monotonic() + 30
        while not Path("/state/gopgroute.json").exists():
            if time.monotonic() > deadline:
                raise RuntimeError("Database preparation deadline exceeded")
            time.sleep(0.1)
        os.execve(  # nosec B606 # Fixed image binary; guardian validates the speaker.
            "/usr/local/bin/gopgroute",
            ["gopgroute", "-config", "/state/gopgroute.json"],
            {
                key: value
                for key, value in os.environ.items()
                if not key.startswith("PG")
            },
        )
    else:
        raise SystemExit("Unknown isolated lab role")
