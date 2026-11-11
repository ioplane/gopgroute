# /// script
# requires-python = ">=3.13"
# dependencies = ["psycopg2-binary==2.9.10"]
# ///
"""Client observations through FRR; nonzero exit on any failed assertion."""

from __future__ import annotations

import json
from contextlib import closing
import subprocess  # nosec B404 # Fixed dig command, no shell or external argv.
import sys
import time
from pathlib import Path

import psycopg2

WRITE = "198.19.255.10"
READ = "198.19.255.11"


def connect(host: str):
    return psycopg2.connect(
        host=host,
        port=6432,
        dbname="pdns",
        user="pdns",
        password=Path("/run/lab/password").read_text().strip(),
        connect_timeout=2,
        sslmode="verify-full",
        sslrootcert="/run/lab/ca.crt",
    )


def observe() -> dict:
    result = {}
    for label, host in (("write", WRITE), ("read", READ)):
        with (
            closing(connect(host)) as connection,
            connection,
            connection.cursor() as cursor,
        ):
            cursor.execute(
                "SELECT pg_is_in_recovery(), inet_server_addr()::text, pg_postmaster_start_time()::text"
            )
            recovery, address, started = cursor.fetchone()
            if recovery != (label == "read"):
                raise RuntimeError(f"Wrong role behind {label}: {recovery}")
            result[label] = {"address": address, "postmaster_started": started}
    response = subprocess.run(  # nosec B603 # Literal dig argv with bounded timeout.
        [
            "/usr/bin/dig",
            "@198.19.0.10",
            "-p",
            "5300",
            "probe.ha.example.test",
            "A",
            "+short",
            "+time=2",
            "+tries=1",
        ],
        capture_output=True,
        text=True,
        check=True,
        timeout=5,
    ).stdout.strip()
    if response != "203.0.113.42":
        raise RuntimeError(f"Unexpected authoritative response: {response!r}")
    result["dns"] = response
    return result


def receipt(identifier: int) -> dict:
    payload = f"ack-{identifier}"
    with (
        closing(connect(WRITE)) as connection,
        connection,
        connection.cursor() as cursor,
    ):
        cursor.execute(
            "INSERT INTO lab_receipts(id,payload) VALUES (%s,%s)", (identifier, payload)
        )
    return {"id": identifier, "payload": payload, "acknowledged": True}


def verify(expected: list[int]) -> dict:
    if not expected or len(set(expected)) != len(expected):
        raise ValueError("Supply distinct acknowledged receipt IDs")
    with (
        closing(connect(WRITE)) as connection,
        connection,
        connection.cursor() as cursor,
    ):
        cursor.execute("SELECT id,payload FROM lab_receipts ORDER BY id")
        rows = cursor.fetchall()
        required = [
            (identifier, f"ack-{identifier}") for identifier in sorted(expected)
        ]
        if rows != required:
            raise RuntimeError(
                f"Acknowledged receipts differ: expected {required!r}, got {rows!r}"
            )
        return {"rows": rows, "acknowledged_receipts_verified": len(required)}


if __name__ == "__main__":
    mode = sys.argv[1] if len(sys.argv) > 1 else "observe"
    started = time.monotonic()
    if mode == "observe":
        value = observe()
    elif mode == "receipt":
        value = receipt(int(sys.argv[2]))
    elif mode == "verify":
        value = verify([int(identifier) for identifier in sys.argv[2:]])
    else:
        raise SystemExit("Unknown client check")
    value["elapsed_seconds"] = time.monotonic() - started
    print(json.dumps(value))
