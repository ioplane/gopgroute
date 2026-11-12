# /// script
# requires-python = ">=3.13"
# dependencies = []
# ///
"""Negative boundary checks; no containers, services or credentials required."""

from __future__ import annotations

import copy
import errno
import http.client
import json
import os
import subprocess
import sys
import types
import unittest
from unittest.mock import MagicMock, patch

import probe


class ProbeTests(unittest.TestCase):
    def test_startup_identity_requires_one_child_of_the_current_postmaster(
        self,
    ) -> None:
        def stat(pid: int, parent: int, birth: str) -> bytes:
            return (
                f"{pid} (postgres) S {parent} ".encode()
                + (" ".join(["0"] * 17 + [birth])).encode()
            )

        files = {
            "/state/pg/postmaster.pid": b"10\n",
            "/proc/10/stat": stat(10, 1, "100"),
            "/proc/10/task/10/children": b"42",
            "/proc/42/cmdline": b"postgres: gopgroute-lab: startup recovering WAL\x00",
            "/proc/42/stat": stat(42, 10, "200"),
        }
        with patch.object(probe, "read", side_effect=lambda p: files[str(p)]):
            value = probe.startup()
            self.assertEqual(value["pid"], 42)
            self.assertEqual(value["start_time"], "200")
            self.assertEqual(value["postmaster_start_time"], "100")
            files["/proc/42/stat"] = stat(42, 11, "200")
            with self.assertRaises(probe.ProbeError):
                probe.startup()
            files["/proc/10/task/10/children"] = b""
            with self.assertRaises(probe.ProbeError):
                probe.startup()
            files["/proc/10/task/10/children"] = b"42 43"
            files["/proc/43/cmdline"] = files["/proc/42/cmdline"]
            with self.assertRaises(probe.ProbeError):
                probe.startup()
            files["/proc/10/task/10/children"] = b"42"
            files["/proc/42/stat"] = stat(42, 10, "200")
            with (
                patch.object(probe, "process_start", side_effect=["100", "201"]),
                self.assertRaises(probe.ProbeError),
            ):
                probe.startup()

    def test_fencing_envelope_keeps_outer_mode_and_worker_sample_time(self) -> None:
        with patch.object(
            probe,
            "bounded_fencing",
            return_value={"mode": "fencing-sql", "observed_at": "sample"},
        ):
            value = probe.fencing()
        self.assertEqual(value["mode"], "fencing")
        self.assertEqual(value["observed_at"], "sample")

    def test_frozen_fencing_worker_is_reaped_without_claiming_fence(self) -> None:
        value = probe.bounded_fencing(
            [
                sys.executable,
                "-c",
                "import os,signal;os.kill(os.getpid(),signal.SIGSTOP)",
            ],
            timeout=0.1,
        )
        self.assertEqual(value["state"], "unknown")
        self.assertTrue(value["execution_stopped"])
        self.assertEqual(value["error"], "WorkerTimeout")
        with self.assertRaises(ProcessLookupError):
            os.kill(value["worker_pid"], 0)

    def test_fencing_worker_failure_never_proves_refusal(self) -> None:
        value = probe.bounded_fencing([sys.executable, "-c", "raise SystemExit(1)"])
        self.assertEqual(value["state"], "unknown")
        self.assertTrue(value["execution_stopped"])

    def test_completed_worker_must_return_a_consistent_database_observation(
        self,
    ) -> None:
        value = probe.bounded_fencing(
            [sys.executable, "-c", 'print(\'{"state":"demoted","sql_accepts":true}\')']
        )
        self.assertEqual(value["state"], "demoted")
        self.assertTrue(value["execution_stopped"])
        for raw in ('{"state":"demoted"}', '{"state":"refused","sql_accepts":true}'):
            with self.subTest(raw=raw), self.assertRaises(probe.ProbeError):
                probe.bounded_fencing([sys.executable, "-c", f"print({raw!r})"])

    def test_unresolved_worker_remains_unknown_after_kill_request(self) -> None:
        child = MagicMock()
        child.pid = 123
        child.communicate.side_effect = subprocess.TimeoutExpired("worker", 1)
        with (
            patch.object(probe.subprocess, "Popen", return_value=child),
            patch.object(probe, "process_start", return_value="42"),
        ):
            value = probe.bounded_fencing(["fixed-worker"])
        self.assertEqual(value["state"], "unknown")
        self.assertFalse(value["execution_stopped"])
        self.assertEqual(value["worker_start_time"], "42")
        child.kill.assert_called_once()

    def test_fencing_identity_and_kill_failures_retain_unknown_execution(self) -> None:
        child = MagicMock()
        child.pid = 123
        child.kill.side_effect = PermissionError("private")
        child.communicate.side_effect = subprocess.TimeoutExpired("worker", 1)
        with (
            patch.object(probe.subprocess, "Popen", return_value=child),
            patch.object(
                probe, "process_start", side_effect=PermissionError("private")
            ),
        ):
            value = probe.bounded_fencing(["fixed-worker"])
        self.assertEqual(value["worker_pid"], 123)
        self.assertIsNone(value["worker_start_time"])
        self.assertFalse(value["execution_stopped"])
        self.assertEqual(value["state"], "unknown")
        self.assertNotIn("private", json.dumps(value))

    def test_duplicate_and_nonfinite_json_rejected(self) -> None:
        for raw in (b'{"x":1,"x":2}', b'{"x":NaN}', b'{"x":Infinity}'):
            with self.subTest(raw=raw), self.assertRaises(probe.ProbeError):
                probe.decode(raw)

    def test_invalid_and_overflow_json_rejected(self) -> None:
        for raw in (b"{", b"x" * (probe.LIMIT + 1)):
            with (
                self.subTest(length=len(raw)),
                self.assertRaises((ValueError, probe.ProbeError)),
            ):
                probe.decode(raw)

    def test_boolean_not_integer_and_missing_field(self) -> None:
        for value in ({"x": True}, {}, [], {"x": "1"}):
            with self.subTest(value=value), self.assertRaises(probe.ProbeError):
                probe.field(value, "x", int)

    def test_config_admits_only_the_two_explicit_fixture_journal_roots(self) -> None:
        cfg = {
            "next_hop": probe.IPS[1],
            "runtime_dir": "/run/gopgroute",
            "speaker_binary": "/usr/local/bin/gobgpd",
            **probe.VIPS,
        }
        for root in ("/state/routes", "/state/routes-lease-v1"):
            cfg["journal_dir"] = root
            with patch.object(probe, "read", return_value=json.dumps(cfg).encode()):
                self.assertEqual(probe.config()[1], 1)
        cfg["journal_dir"] = "/state/routes-unadmitted"
        with (
            patch.object(probe, "read", return_value=json.dumps(cfg).encode()),
            self.assertRaises(probe.ProbeError),
        ):
            probe.config()

    def test_active_generation_comes_from_exact_open_adapter_lock(self) -> None:
        root = probe.Path("/state/routes-lease-v1")
        generation = "i-" + "a" * 32
        descriptor = MagicMock()
        descriptor.readlink.return_value = root / generation / "lock"
        descriptor.samefile.return_value = True
        with patch.object(probe.Path, "iterdir", return_value=[descriptor]):
            self.assertEqual(
                probe.active_generation(probe.Path("/proc/10"), root), generation
            )
        for entries in ([], [descriptor, descriptor]):
            with (
                patch.object(probe.Path, "iterdir", return_value=entries),
                self.assertRaises(probe.ProbeError),
            ):
                probe.active_generation(probe.Path("/proc/10"), root)
        descriptor.samefile.return_value = False
        with (
            patch.object(probe.Path, "iterdir", return_value=[descriptor]),
            self.assertRaises(probe.ProbeError),
        ):
            probe.active_generation(probe.Path("/proc/10"), root)

    def test_http_deadline_and_response_size(self) -> None:
        connection = MagicMock()
        response = connection.getresponse.return_value
        response.status = 503
        response.read.return_value = b'{"state":"running"}'
        with (
            patch.object(http.client, "HTTPConnection", return_value=connection),
            patch.object(probe.signal, "signal"),
            patch.object(probe.signal, "setitimer") as timer,
        ):
            self.assertEqual(probe.request("127.0.0.1", 8008, "/replica")[0], 503)
            response.read.assert_called_once_with(probe.LIMIT + 1)
            self.assertEqual(timer.call_args_list[0].args[1], 2)
            self.assertEqual(timer.call_args_list[-1].args[1], 0)
        connection.close.assert_called_once()

    def test_http_failure_closes_connection_and_cancels_timer(self) -> None:
        connection = MagicMock()
        connection.getresponse.side_effect = TimeoutError
        with (
            patch.object(http.client, "HTTPConnection", return_value=connection),
            patch.object(probe.signal, "signal"),
            patch.object(probe.signal, "setitimer") as timer,
            self.assertRaises(TimeoutError),
        ):
            probe.request("127.0.0.1", 8008, "/patroni")
        connection.close.assert_called_once()
        self.assertEqual(timer.call_args.args[1], 0)

    def test_http_redirect_not_followed(self) -> None:
        connection = MagicMock()
        connection.getresponse.return_value.status = 302
        connection.getresponse.return_value.read.return_value = b"{}"
        with (
            patch.object(http.client, "HTTPConnection", return_value=connection),
            patch.object(probe.signal, "signal"),
            patch.object(probe.signal, "setitimer"),
        ):
            self.assertEqual(probe.request("127.0.0.1", 8008, "/patroni"), (302, {}))
        connection.request.assert_called_once()

    @staticmethod
    def processes() -> list[dict]:
        return [
            {
                "pid": 10,
                "ppid": 1,
                "exe_sha256": "guardian",
                "leases": [{"fd": 7}, {"fd": 8}],
            },
            {
                "pid": 11,
                "ppid": 10,
                "exe_sha256": "guardian",
                "leases": [{"fd": 4}, {"fd": 5}],
            },
            {
                "pid": 12,
                "ppid": 10,
                "exe_sha256": "speaker",
                "leases": [{"fd": 3}, {"fd": 4}],
            },
        ]

    def test_process_identity_uses_parent_executable_and_fd_not_comm(self) -> None:
        rows = self.processes()
        rows[2]["comm"] = "6"
        self.assertEqual(
            probe.classify_processes(rows, "guardian", "speaker")["speaker"]["pid"], 12
        )

    def test_missing_duplicate_wrong_parent_or_fd_rejected(self) -> None:
        examples = [self.processes()[:2], self.processes() + self.processes()[:1]]
        for key, value in (
            ("ppid", 1),
            ("exe_sha256", "foreign"),
            ("leases", [{"fd": 3}, {"fd": 4}]),
        ):
            rows = self.processes()
            rows[1][key] = value
            examples.append(rows)
        for rows in examples:
            with self.subTest(rows=rows), self.assertRaises(probe.ProbeError):
                probe.classify_processes(rows, "guardian", "speaker")

    @staticmethod
    def neighbor_rows() -> dict:
        return {
            address: {
                "bgpState": "Established",
                "gracefulRestartInfo": {"localGrMode": "Disable"},
                "addressFamilyInfo": {"ipv4Unicast": {"acceptedPrefixCounter": 1}},
            }
            for address in ("192.0.2.10", "198.18.0.12", "198.18.0.13")
        }

    def test_unhealthy_peer_is_observation_not_success_default(self) -> None:
        value = self.neighbor_rows()
        value["192.0.2.10"]["bgpState"] = "Idle"
        self.assertEqual(probe.peers(value)[0]["state"], "Idle")
        self.assertEqual(probe.peers(value)[0]["local_gr_mode"], "Disable")
        self.assertEqual(probe.peers(value)[0]["accepted_prefixes"], 1)

    def test_missing_peer_or_gr_fields_rejected(self) -> None:
        cases = [{}, {"p": {}}, self.neighbor_rows()]
        del cases[-1]["192.0.2.10"]["gracefulRestartInfo"]
        for value in cases:
            with self.subTest(value=value), self.assertRaises(probe.ProbeError):
                probe.peers(value)

    def test_installation_requires_rib_and_kernel_agreement(self) -> None:
        prefix = probe.VIPS["write"]
        kernel = [
            {"dst": prefix.removesuffix("/32"), "gateway": "192.0.2.10", "dev": "eth0"}
        ]
        result = probe.fib(
            {
                prefix: [
                    {"installed": True, "nexthops": [{"ip": "192.0.2.10", "fib": True}]}
                ]
            },
            prefix,
            kernel,
        )
        self.assertTrue(result["installed"])
        self.assertTrue(result["rib_installed"])
        self.assertTrue(result["kernel_installed"])
        empty = probe.fib({}, prefix, [])
        self.assertFalse(empty["installed"])
        self.assertFalse(empty["rib_installed"])
        self.assertFalse(empty["kernel_installed"])

    def test_rib_kernel_mismatch_and_residual_routes_are_rejected(self) -> None:
        prefix = probe.VIPS["write"]
        rib = {
            prefix: [
                {"installed": True, "nexthops": [{"ip": "192.0.2.10", "fib": True}]}
            ]
        }
        kernel = [{"dst": prefix, "gateway": "192.0.2.10", "dev": "eth0"}]
        for rows, routes in (
            (rib, []),
            ({}, kernel),
            (rib, [{**kernel[0], "gateway": "192.0.2.11"}]),
            (rib, [{**kernel[0], "type": "blackhole"}]),
        ):
            with (
                self.subTest(rib=rows, kernel=routes),
                self.assertRaises(probe.ProbeError),
            ):
                probe.fib(rows, prefix, routes)

    @staticmethod
    def origin_rows() -> dict:
        prefix = probe.VIPS["read"]
        return {
            "routes": {
                prefix: [
                    {
                        "valid": True,
                        "bestpath": False,
                        "prefix": prefix.removesuffix("/32"),
                        "prefixLen": 32,
                        "network": prefix,
                        "peerId": probe.IPS[1],
                        "path": "64521",
                        "nexthops": [{"ip": probe.IPS[1], "afi": "ipv4", "used": True}],
                    }
                ]
            },
            "totalRoutes": 1,
        }

    def test_local_origins_are_independent_of_selected_bestpath(self) -> None:
        self.assertEqual(
            probe.origin_prefixes(self.origin_rows(), probe.IPS[1]),
            [probe.VIPS["read"]],
        )
        self.assertEqual(
            probe.origin_prefixes({"routes": {}, "totalRoutes": 0}, probe.IPS[1]), []
        )

    def test_malformed_foreign_or_wrong_peer_origin_is_rejected(self) -> None:
        for key, replacement in (
            ("peerId", probe.IPS[2]),
            ("network", probe.VIPS["write"]),
            ("valid", False),
            ("path", "64522"),
            ("nexthops", []),
        ):
            value = self.origin_rows()
            value["routes"][probe.VIPS["read"]][0][key] = replacement
            with self.subTest(key=key), self.assertRaises(probe.ProbeError):
                probe.origin_prefixes(value, probe.IPS[1])
        for value in (
            {},
            {"routes": [], "totalRoutes": 0},
            {"routes": {"192.0.2.0/24": []}, "totalRoutes": 1},
        ):
            with self.subTest(value=value), self.assertRaises(probe.ProbeError):
                probe.origin_prefixes(value, probe.IPS[1])

    def test_missing_fib_flag_or_foreign_prefix_rejected(self) -> None:
        prefix = probe.VIPS["write"]
        for value, kernel in (
            ({prefix: [{"nexthops": []}]}, []),
            ({"192.0.2.0/24": []}, []),
            ({}, [{"dst": "192.0.2.0/24"}]),
            ({prefix: [{"installed": True, "nexthops": [{"ip": "192.0.2.10"}]}]}, []),
        ):
            with self.subTest(value=value), self.assertRaises(probe.ProbeError):
                probe.fib(value, prefix, kernel)

    @staticmethod
    def member_rows() -> dict:
        return {
            "members": [
                {
                    "ID": str(i),
                    "name": f"db{i}",
                    "peerURLs": [f"https://{address}:2380"],
                    "clientURLs": [f"https://{address}:2379"],
                }
                for i, address in probe.IPS.items()
            ]
        }

    def test_proto_default_is_learner_false_only_after_valid_response(self) -> None:
        rows = probe.etcd_members(self.member_rows())
        self.assertEqual(len(rows), 3)
        self.assertTrue(all(not row["is_learner"] for row in rows))

    def test_bad_etcd_members_fail_closed(self) -> None:
        cases = [{}, {"members": []}]
        for key, value in (
            ("ID", "0"),
            ("isLearner", "false"),
            ("peerURLs", ["http://192.0.2.10:2380"]),
        ):
            rows = self.member_rows()
            rows["members"][0][key] = value
            cases.append(rows)
        duplicate = self.member_rows()
        duplicate["members"][1] = copy.deepcopy(duplicate["members"][0])
        cases.append(duplicate)
        for value in cases:
            with self.subTest(value=value), self.assertRaises(probe.ProbeError):
                probe.etcd_members(value)

    def test_patroni_projection_omits_sensitive_unknown_fields(self) -> None:
        status = {
            "role": "primary",
            "state": "running",
            "postmaster_start_time": "2026-11-11T00:00:00Z",
            "password": "fixture-private-value",
        }
        cluster = {
            "members": [
                {"name": f"db{i}", "role": "replica", "state": "streaming"}
                for i in range(1, 4)
            ]
        }
        result = probe.patroni(status, cluster, {"patroni": 200})
        self.assertNotIn("fixture-private-value", json.dumps(result))
        del cluster["members"][0]["role"]
        with self.assertRaises(probe.ProbeError):
            probe.patroni(status, cluster, {})

    def test_process_start_ignores_spaces_and_parentheses_in_comm(self) -> None:
        fields = ["S", *["0"] * 18, "123456", "0"]
        raw = ("42 (binary (name) 6) " + " ".join(fields)).encode()
        self.assertEqual(probe.start_time(raw, 42), "123456")
        for invalid in (raw.replace(b"42 (", b"43 ("), b"42 (truncated) S 0"):
            with self.subTest(invalid=invalid), self.assertRaises(probe.ProbeError):
                probe.start_time(invalid, 42)

    @staticmethod
    def fake_pg() -> tuple[types.SimpleNamespace, MagicMock, MagicMock]:
        class DatabaseError(Exception):
            pass

        connection = MagicMock()
        cursor = connection.cursor.return_value.__enter__.return_value
        module = types.SimpleNamespace(
            Error=DatabaseError, connect=MagicMock(return_value=connection)
        )
        return module, connection, cursor

    def test_client_continues_after_write_connection_failure(self) -> None:
        pg, connection, cursor = self.fake_pg()
        pg.connect.side_effect = [pg.Error("private DSN must not escape"), connection]
        cursor.fetchone.return_value = (True, "started")
        dig = MagicMock(stdout="203.0.113.42\n")
        with (
            patch.dict(sys.modules, {"psycopg2": pg}),
            patch.object(probe.Path, "read_text", return_value="private"),
            patch.object(probe.subprocess, "run", return_value=dig),
        ):
            value = probe.client()
        self.assertFalse(value["ok"])
        self.assertFalse(value["checks"]["write"]["ok"])
        self.assertTrue(value["checks"]["read"]["ok"])
        self.assertTrue(value["checks"]["dns"]["ok"])
        self.assertNotIn("private", json.dumps(value))
        self.assertNotIn("options", pg.connect.call_args.kwargs)
        cursor.execute.assert_any_call("SET LOCAL statement_timeout='1500ms'")

    def test_ledger_limit_and_read_only_transaction(self) -> None:
        pg, _, cursor = self.fake_pg()
        with (
            patch.dict(sys.modules, {"psycopg2": pg}),
            patch.object(probe.Path, "read_text", return_value="private"),
        ):
            cursor.fetchall.return_value = [(1, "ack-1")]
            self.assertEqual(probe.ledger()["rows"], [(1, "ack-1")])
            cursor.execute.assert_any_call("SET TRANSACTION READ ONLY")
            self.assertNotIn("options", pg.connect.call_args.kwargs)
            cursor.fetchall.return_value = [(1, "ack-1")] * 10001
            with self.assertRaises(probe.ProbeError):
                probe.ledger()

    def test_fencing_observes_read_acceptance_without_writing(self) -> None:
        pg, _, cursor = self.fake_pg()
        cursor.fetchone.return_value = (False,)
        with (
            patch.dict(sys.modules, {"psycopg2": pg}),
            patch.object(probe.socket, "create_connection"),
        ):
            value = probe.fencing_sql()
            self.assertTrue(value["sql_accepts"])
            self.assertEqual(value["state"], "primary")
            cursor.execute.assert_called_once_with("SELECT pg_is_in_recovery()")
            cursor.fetchone.return_value = (True,)
            self.assertEqual(probe.fencing_sql()["state"], "demoted")
            pg.connect.side_effect = pg.Error("private")
            value = probe.fencing_sql()
        self.assertIsNone(value["sql_accepts"])
        self.assertEqual(value["state"], "unknown")
        self.assertNotIn("private", json.dumps(value))

    def test_fencing_refused_requires_native_backend_econnrefused(self) -> None:
        pg, _, _ = self.fake_pg()
        with (
            patch.dict(sys.modules, {"psycopg2": pg}),
            patch.object(
                probe.socket,
                "create_connection",
                side_effect=ConnectionRefusedError(errno.ECONNREFUSED, "private"),
            ) as tcp,
        ):
            value = probe.fencing_sql()
        self.assertEqual(value["state"], "refused")
        self.assertFalse(value["sql_accepts"])
        self.assertEqual(value["refusal_errno"], errno.ECONNREFUSED)
        self.assertNotIn("private", json.dumps(value))
        tcp.assert_called_once_with(("127.0.0.1", 5432), timeout=2)
        pg.connect.assert_not_called()

    def test_fencing_timeouts_auth_query_and_os_errors_never_mean_fenced(self) -> None:
        pg, _, cursor = self.fake_pg()
        for error in (
            TimeoutError(),
            PermissionError(),
            OSError(errno.EIO, "Connection refused"),
            ConnectionRefusedError("Connection refused"),
        ):
            with (
                self.subTest(error=type(error).__name__),
                patch.dict(sys.modules, {"psycopg2": pg}),
                patch.object(probe.socket, "create_connection", side_effect=error),
            ):
                value = probe.fencing_sql()
                self.assertEqual(value["state"], "unknown")
                self.assertIsNone(value["sql_accepts"])
        for error in (
            pg.Error("auth failure private"),
            pg.Error("query timeout private"),
            TimeoutError(),
            OSError(errno.ECONNREFUSED, "private"),
        ):
            cursor.execute.side_effect = error
            with (
                self.subTest(query_error=type(error).__name__),
                patch.dict(sys.modules, {"psycopg2": pg}),
                patch.object(probe.socket, "create_connection"),
            ):
                value = probe.fencing_sql()
                self.assertEqual(value["state"], "unknown")
                self.assertIsNone(value["sql_accepts"])
                self.assertNotIn("private", json.dumps(value))


if __name__ == "__main__":
    unittest.main()
