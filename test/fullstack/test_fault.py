# /// script
# requires-python = ">=3.13"
# dependencies = []
# ///
"""Fault guards and real pidfd signals against test-owned container children."""
# ruff: noqa: PLR2004, TRY003
# Literal synthetic process identities and failures are fixture data.

from __future__ import annotations

import json
import os
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import MagicMock, call, patch
from typing import cast

import fault


class FaultTests(unittest.TestCase):
    def test_actual_child_stop_resume_and_mismatch(self) -> None:
        child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(60)"])
        try:
            start = fault.identity(child.pid)
            with self.assertRaises(ValueError):
                fault.send(child.pid, start + "1", signal.SIGSTOP)
            self.assertEqual(
                fault.send(child.pid, start, signal.SIGSTOP)["signal"], "SIGSTOP"
            )
            deadline = time.monotonic() + 2
            while True:
                state = (
                    fault.Path(f"/proc/{child.pid}/stat")
                    .read_text(encoding="utf-8")
                    .rsplit(") ", 1)[1]
                    .split()[0]
                )
                if state == "T":
                    break
                if time.monotonic() >= deadline:
                    self.fail("Owned child did not enter STOP state")
                time.sleep(0.005)
            result = fault.pg_resume([{"pid": child.pid, "start_time": start}])
            self.assertEqual(result[0]["signal"], "SIGCONT")
            self.assertIsNone(child.poll())
        finally:
            child.kill()
            child.wait(timeout=3)

    def test_reused_pid_never_receives_cont(self) -> None:
        with (
            patch.object(fault, "identity", return_value="200"),
            patch.object(os, "pidfd_open") as opened,
            patch.object(signal, "pidfd_send_signal") as sent,
        ):
            self.assertTrue(fault.send(42, "100", signal.SIGCONT)["gone"])
        opened.assert_not_called()
        sent.assert_not_called()

    def test_identity_changed_after_open_closes_fd_without_signal(self) -> None:
        for signum in (signal.SIGSTOP, signal.SIGCONT):
            with (
                self.subTest(signal=signum),
                patch.object(fault, "identity", side_effect=["100", "200"]),
                patch.object(os, "pidfd_open", return_value=80),
                patch.object(os, "close") as closed,
                patch.object(signal, "pidfd_send_signal") as sent,
            ):
                if signum == signal.SIGCONT:
                    self.assertTrue(fault.send(42, "100", signum)["gone"])
                else:
                    with self.assertRaises(ValueError):
                        fault.send(42, "100", signum)
                sent.assert_not_called()
                closed.assert_called_once_with(80)

    def test_disappeared_process_after_open_is_gone_on_resume(self) -> None:
        with (
            patch.object(fault, "identity", side_effect=["100", FileNotFoundError()]),
            patch.object(os, "pidfd_open", return_value=80),
            patch.object(os, "close") as closed,
        ):
            self.assertTrue(fault.send(42, "100", signal.SIGCONT)["gone"])
        closed.assert_called_once_with(80)

    def test_invalid_identities_validated_before_any_resume(self) -> None:
        valid = {"pid": 42, "start_time": "100"}
        invalids = [
            None,
            {},
            [],
            [valid, valid],
            [{"pid": True, "start_time": "100"}],
            [valid, {"pid": 1, "start_time": "100"}],
            [{"pid": 42, "start_time": 100}],
            [{"pid": 42, "start_time": "100\n"}],
            [{"pid": 42, "start_time": "100", "extra": 1}],
        ]
        for items in invalids:
            with (
                self.subTest(items=items),
                patch.object(fault, "send") as sent,
                self.assertRaises(ValueError),
            ):
                fault.pg_resume(cast(list[dict], items))
            sent.assert_not_called()

    def test_resume_attempts_every_identity_and_aggregates_failures(self) -> None:
        items = [{"pid": pid, "start_time": str(pid)} for pid in (10, 11, 12)]
        with (
            patch.object(
                fault, "send", side_effect=[PermissionError(), OSError(), {"pid": 10}]
            ) as sent,
            self.assertRaises(ExceptionGroup) as caught,
        ):
            fault.pg_resume(items)
        self.assertEqual(len(caught.exception.exceptions), 2)
        self.assertEqual([entry.args[0] for entry in sent.call_args_list], [12, 11, 10])

    def test_partial_stop_rollback_attempts_all_after_multiple_failures(self) -> None:
        def text(path: fault.Path, *args: object, **kwargs: object) -> str:
            del args, kwargs
            return (
                "10\n"
                if str(path).endswith("/pg/postmaster.pid")
                else "11 12"
                if str(path).endswith("10/children")
                else ""
            )

        def send(pid: int, start: str, signum: signal.Signals) -> dict:
            if pid == 11 or (pid == 12 and signum == signal.SIGCONT):
                raise PermissionError("synthetic partial failure")
            return {"pid": pid, "start_time": start}

        with (
            tempfile.TemporaryDirectory() as directory,
            patch.object(fault, "STATE", fault.Path(directory)),
            patch.object(fault.Path, "read_text", autospec=True, side_effect=text),
            patch.object(fault, "identity", side_effect=str),
            patch.object(fault, "send", side_effect=send) as sent,
            self.assertRaises(ExceptionGroup) as caught,
        ):
            fault.pg_stop("a" * 32)
        self.assertEqual(len(caught.exception.exceptions), 2)
        self.assertEqual(
            sent.call_args_list[-3:],
            [
                call(11, "11", signal.SIGCONT),
                call(12, "12", signal.SIGCONT),
                call(10, "10", signal.SIGCONT),
            ],
        )

    def test_closed_signal_and_sql_operations(self) -> None:
        for signum in (signal.SIGTERM, 19, "STOP"):
            with self.subTest(signal=signum), self.assertRaises(ValueError):
                fault.send(42, "100", cast(signal.Signals, signum))
        with self.assertRaises(ValueError):
            fault.sql("DROP TABLE lab_receipts")
        for args in (
            ["signal", "42", "100", "TERM"],
            ["pg-resume"],
            ["replay", "other"],
            ["switch", "db1", "foreign"],
        ):
            with self.subTest(args=args), self.assertRaises(ValueError):
                fault.main(args)

    def test_api_rejects_unknown_paths_methods_members_and_extra_fields(self) -> None:
        cases = [
            ("/config", "POST", {"synchronous_node_count": 1}),
            ("/config", "PATCH", {"synchronous_node_count": True}),
            ("/config", "PATCH", {"synchronous_node_count": 1, "other": 1}),
            ("/switchover", "POST", {"leader": "db1", "candidate": "db1"}),
            ("/switchover", "POST", {"leader": "db1", "candidate": "foreign"}),
            ("http://foreign", "PATCH", {}),
        ]
        for path, method, body in cases:
            with (
                self.subTest(path=path, body=body),
                patch.object(fault.http.client, "HTTPConnection") as opened,
                self.assertRaises(ValueError),
            ):
                fault.api(path, method, body)
            opened.assert_not_called()

    def test_api_never_follows_redirect_or_returns_config_payload(self) -> None:
        connection = MagicMock()
        response = connection.getresponse.return_value
        response.status = 200
        response.read.return_value = json.dumps(
            {
                "private": "must-not-escape",
                "synchronous_node_count": 1,
                "synchronous_mode": True,
                "synchronous_mode_strict": True,
            }
        ).encode()
        with patch.object(fault.http.client, "HTTPConnection", return_value=connection):
            result = fault.api("/config", "PATCH", {"synchronous_node_count": 1})
            self.assertNotIn("must-not-escape", json.dumps(result))
            response.status = 302
            with self.assertRaises(ValueError):
                fault.api("/config", "PATCH", {"synchronous_node_count": 1})
            response.status = 200
            response.read.return_value = b"x" * 65537
            with self.assertRaises(ValueError):
                fault.api("/config", "PATCH", {"synchronous_node_count": 1})
        self.assertEqual(connection.request.call_count, 4)
        self.assertEqual(connection.close.call_count, 3)

    def test_cluster_read_accepts_partial_membership_without_mutation(self) -> None:
        connection = MagicMock()
        response = connection.getresponse.return_value
        response.status = 200
        response.read.return_value = b'{"members":[{"name":"db1"}]}'
        with patch.object(fault.http.client, "HTTPConnection", return_value=connection):
            self.assertEqual(fault.main(["cluster"]), {"members": [{"name": "db1"}]})
            response.read.return_value = b'{"members":null}'
            with self.assertRaises(ValueError):
                fault.main(["cluster"])
        self.assertEqual(connection.request.call_args.args[:2], ("GET", "/cluster"))
        self.assertIsNone(connection.request.call_args.kwargs["body"])

    def test_replication_profile_requires_exact_effective_values(self) -> None:
        connection = MagicMock()
        response = connection.getresponse.return_value
        response.status = 200
        for timeout in ("10s", "60s"):
            wanted = {"wal_sender_timeout": timeout, "wal_receiver_timeout": timeout}
            config = {
                "postgresql": {"parameters": wanted | {"private": "must-not-escape"}},
                "synchronous_mode": True,
                "synchronous_mode_strict": True,
            }
            response.read.return_value = json.dumps(config).encode()
            with patch.object(
                fault.http.client, "HTTPConnection", return_value=connection
            ):
                result = fault.main(["replication-timeouts", timeout])
                self.assertEqual(result["parameters"], wanted)
                self.assertNotIn("must-not-escape", json.dumps(result))
                config["postgresql"] = {"parameters": {"wal_sender_timeout": timeout}}
                response.read.return_value = json.dumps(config).encode()
                with self.assertRaises(ValueError):
                    fault.main(["replication-timeouts", timeout])
        for value in ("0", "1s", "600s"):
            with self.assertRaises(ValueError):
                fault.main(["replication-timeouts", value])

    def test_campaign_lock_excludes_overlap_and_preserves_wrong_owner_nonce(
        self,
    ) -> None:
        nonce, other = "a" * 32, "b" * 32
        with (
            tempfile.TemporaryDirectory() as directory,
            patch.object(fault, "EVIDENCE", fault.Path(directory)),
        ):
            self.assertEqual(
                fault.main(["campaign-acquire", nonce]), {"acquired": True}
            )
            lock = fault.Path(directory) / "campaign.lock"
            self.assertEqual(lock.stat().st_mode & 0o777, 0o700)
            self.assertEqual((lock / "nonce").stat().st_mode & 0o777, 0o600)
            with self.assertRaises(FileExistsError):
                fault.main(["campaign-acquire", other])
            with self.assertRaises(ValueError):
                fault.main(["campaign-release", other])
            self.assertEqual((lock / "nonce").read_text(encoding="ascii"), nonce)
            self.assertEqual(
                fault.main(["campaign-release", nonce]), {"released": True}
            )
            self.assertFalse(lock.exists())

    def test_campaign_nonce_schema_rejects_before_creating_lock(self) -> None:
        for nonce in ("a" * 31, "A" * 32, "../" + "a" * 32):
            with (
                self.subTest(nonce=nonce),
                patch.object(fault.Path, "mkdir") as create,
                self.assertRaises(ValueError),
            ):
                fault.main(["campaign-acquire", nonce])
            create.assert_not_called()

    def test_lost_stop_reply_recovers_real_child_using_only_token(self) -> None:
        child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(60)"])
        try:
            with (
                tempfile.TemporaryDirectory() as directory,
                patch.object(fault, "STATE", fault.Path(directory)),
            ):
                pg = fault.Path(directory) / "pg"
                pg.mkdir()
                (pg / "postmaster.pid").write_text(str(child.pid), encoding="ascii")
                nonce = "c" * 32
                fault.main(
                    ["pg-stop", nonce]
                )  # Intentionally discard the mutation reply.
                path, _, _ = fault.pause_paths(nonce)
                self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                saved = fault.load_pause(path, nonce)
                self.assertEqual(saved["processes"][0]["pid"], child.pid)
                self.assertTrue(
                    cast(dict, fault.main(["pg-resume-token", nonce]))["resumed"]
                )
                self.assertFalse(path.exists())
                self.assertTrue(
                    cast(dict, fault.main(["pg-resume-token", nonce]))[
                        "no_pending_intent"
                    ]
                )
                with self.assertRaises(FileExistsError):
                    fault.main(["pg-stop", nonce])
                self.assertIsNone(child.poll())
        finally:
            child.kill()
            child.wait(timeout=3)

    def test_manifest_is_durable_before_stop_and_failed_resume_keeps_it(self) -> None:
        nonce = "d" * 32
        with (
            tempfile.TemporaryDirectory() as directory,
            patch.object(fault, "STATE", fault.Path(directory)),
        ):
            pg = fault.Path(directory) / "pg"
            pg.mkdir()
            (pg / "postmaster.pid").write_text("42", encoding="ascii")
            path, _, _ = fault.pause_paths(nonce)

            def send(pid: int, start: str, signum: signal.Signals) -> dict:
                self.assertEqual(
                    fault.load_pause(path, nonce)["processes"],
                    [{"pid": pid, "start_time": start}],
                )
                return {"pid": pid}

            with (
                patch.object(fault, "identity", return_value="100"),
                patch.object(fault, "send", side_effect=send),
                patch.object(fault.Path, "read_text", return_value="42"),
                self.assertRaises(ValueError),
            ):
                # A repeated synthetic child fails tree admission and triggers rollback.
                fault.pg_stop(nonce)
            self.assertFalse(path.exists())
            # A pending manifest survives any failed CONT and can be retried safely.
            with fault.pause_guard("e" * 32, create=True) as (path, staging):
                fault.save_pause(
                    path,
                    staging,
                    {
                        "version": 1,
                        "nonce": "e" * 32,
                        "processes": [{"pid": 42, "start_time": "100"}],
                    },
                    create=True,
                )
            with (
                patch.object(fault, "send", side_effect=PermissionError()),
                self.assertRaises(ExceptionGroup),
            ):
                fault.pg_resume_token("e" * 32)
            self.assertTrue(path.exists())

    def test_manifest_schema_mode_symlink_and_guard_fail_closed(self) -> None:
        nonce = "f" * 32
        with (
            tempfile.TemporaryDirectory() as directory,
            patch.object(fault, "STATE", fault.Path(directory)),
        ):
            self.assertTrue(fault.pg_resume_token(nonce)["no_pending_intent"])
            path, staging, guard = fault.pause_paths(nonce)
            path.write_text("{}", encoding="ascii")
            with self.assertRaises(ValueError):
                fault.pg_resume_token(nonce)
            path.unlink()
            with fault.pause_guard(nonce, create=True):
                fault.save_pause(
                    path,
                    staging,
                    {"version": 1, "nonce": nonce, "processes": []},
                    create=True,
                )
            path.chmod(0o644)
            with self.assertRaises(ValueError):
                fault.pg_resume_token(nonce)
            path.chmod(0o600)
            path.write_text(
                '{"version":1,"nonce":"wrong","processes":[]}', encoding="ascii"
            )
            with self.assertRaises(ValueError):
                fault.pg_resume_token(nonce)
            path.unlink()
            path.symlink_to(guard)
            with self.assertRaises(OSError):
                fault.pg_resume_token(nonce)

    def test_sync_count_requires_effective_readback_and_strict_mode(self) -> None:
        connection = MagicMock()
        response = connection.getresponse.return_value
        response.status = 200
        for body in (
            {
                "synchronous_node_count": 1.0,
                "synchronous_mode": True,
                "synchronous_mode_strict": True,
            },
            {
                "synchronous_node_count": 2,
                "synchronous_mode": True,
                "synchronous_mode_strict": True,
            },
            {
                "synchronous_node_count": 1,
                "synchronous_mode": True,
                "synchronous_mode_strict": False,
            },
        ):
            response.read.return_value = json.dumps(body).encode()
            with (
                self.subTest(body=body),
                patch.object(
                    fault.http.client, "HTTPConnection", return_value=connection
                ),
                self.assertRaises(ValueError),
            ):
                fault.main(["sync-count", "1"])

    def test_active_stop_guard_blocks_concurrent_resume(self) -> None:
        nonce = "0" * 32
        with (
            tempfile.TemporaryDirectory() as directory,
            patch.object(fault, "STATE", fault.Path(directory)),
        ):
            with fault.pause_guard(nonce, create=True) as (path, staging):
                fault.save_pause(
                    path,
                    staging,
                    {"version": 1, "nonce": nonce, "processes": []},
                    create=True,
                )
                with (
                    patch.object(fault, "send") as sent,
                    self.assertRaises(BlockingIOError),
                ):
                    fault.pg_resume_token(nonce)
                sent.assert_not_called()
            self.assertTrue(fault.pg_resume_token(nonce)["resumed"])

    def test_failed_durable_intent_prevents_stop_signal(self) -> None:
        nonce = "1" * 32
        save = fault.save_pause

        def fail_update(
            path: fault.Path, staging: fault.Path, value: dict, *, create: bool
        ) -> None:
            if not create:
                raise OSError("synthetic intent persistence failure")
            save(path, staging, value, create=create)

        with (
            tempfile.TemporaryDirectory() as directory,
            patch.object(fault, "STATE", fault.Path(directory)),
        ):
            pg = fault.Path(directory) / "pg"
            pg.mkdir()
            (pg / "postmaster.pid").write_text("42", encoding="ascii")
            with (
                patch.object(fault, "identity", return_value="100"),
                patch.object(fault, "save_pause", side_effect=fail_update),
                patch.object(fault, "send") as sent,
                self.assertRaises(OSError),
            ):
                fault.pg_stop(nonce)
            self.assertTrue(
                all(entry.args[2] == signal.SIGCONT for entry in sent.call_args_list)
            )
            self.assertFalse(fault.pause_paths(nonce)[0].exists())

    @staticmethod
    def signal_marker(nonce: str) -> dict:
        return {
            "version": 1,
            "nonce": nonce,
            "phase": "prepared",
            "target": {"pid": 45, "start_time": "90", "signal": "SIGKILL"},
            "injector": {
                "pid": 42,
                "start_time": "100",
                "pid_namespace_inode": fault.Path("/proc/self/ns/pid").stat().st_ino,
            },
        }

    def test_signal_prepared_receipt_precedes_effect_and_token_never_replays(
        self,
    ) -> None:
        nonce = "2" * 32
        with (
            tempfile.TemporaryDirectory() as directory,
            patch.object(fault, "STATE", fault.Path(directory)),
        ):

            def send(pid: int, start: str, signum: signal.Signals) -> dict:
                value = fault.private_json(fault.signal_path(nonce))
                self.assertEqual(cast(dict, value)["phase"], "prepared")
                self.assertEqual(cast(dict, value)["injector"]["pid"], 42)
                self.assertEqual(cast(dict, value)["target"]["signal"], "SIGKILL")
                return {"pid": pid, "start_time": start, "signal": signum.name}

            with (
                patch.object(os, "getpid", return_value=42),
                patch.object(fault, "identity", return_value="100"),
                patch.object(fault, "send", side_effect=send) as effect,
            ):
                self.assertEqual(
                    fault.signal_token(nonce, 45, "90", signal.SIGKILL)["phase"],
                    "returned",
                )
                with self.assertRaises(FileExistsError):
                    fault.signal_token(nonce, 45, "90", signal.SIGKILL)
                effect.assert_called_once()

    def test_signal_write_failure_prevents_effect(self) -> None:
        with (
            tempfile.TemporaryDirectory() as directory,
            patch.object(fault, "STATE", fault.Path(directory)),
            patch.object(os, "getpid", return_value=42),
            patch.object(fault, "identity", return_value="100"),
            patch.object(fault, "save_pause", side_effect=OSError()),
            patch.object(fault, "send") as effect,
            self.assertRaises(OSError),
        ):
            fault.signal_token("3" * 32, 45, "90", signal.SIGKILL)
        effect.assert_not_called()

    def test_real_injector_killed_before_reply_is_quiescent_from_durable_marker(
        self,
    ) -> None:
        nonce = "4" * 32
        code = (
            "import fault,os,signal,sys;from pathlib import Path;"
            "fault.STATE=Path(sys.argv[1]);"
            "fault.signal_token(sys.argv[2],os.getpid(),"
            "fault.identity(os.getpid()),signal.SIGKILL)"
        )
        with (
            tempfile.TemporaryDirectory() as directory,
            patch.object(fault, "STATE", fault.Path(directory)),
        ):
            child = subprocess.Popen(  # noqa: S603 # Fixed Python test code and owned temporary directory, no shell.
                [sys.executable, "-c", code, directory, nonce],
                cwd=fault.Path(__file__).parent,
            )
            try:
                self.assertEqual(child.wait(timeout=5), -signal.SIGKILL)
            finally:
                if child.poll() is None:
                    child.kill()
                    child.wait(timeout=3)
            path = fault.signal_path(nonce)
            before = path.read_bytes()
            value = fault.signal_status(nonce)
            self.assertEqual(value["phase"], "prepared")
            self.assertEqual(value["injector_state"], "gone")
            self.assertTrue(value["injector_quiescent"])
            self.assertFalse(value["effect_confirmed"])
            self.assertEqual(path.read_bytes(), before)

    def test_signal_status_running_stopped_reused_and_namespace_mismatch(self) -> None:
        nonce = "5" * 32
        with (
            tempfile.TemporaryDirectory() as directory,
            patch.object(fault, "STATE", fault.Path(directory)),
        ):
            path = fault.signal_path(nonce)
            value = self.signal_marker(nonce)
            fault.save_pause(path, path.with_suffix(".json.next"), value, create=True)
            for state, quiescent in (("S", False), ("T", False), ("Z", False)):
                text = "42 (injector) " + " ".join([state, *["0"] * 18, "100"])
                with (
                    patch.object(fault, "identity", return_value="100"),
                    patch.object(fault.Path, "read_text", return_value=text),
                ):
                    self.assertEqual(
                        fault.signal_status(nonce)["injector_quiescent"], quiescent
                    )
            with patch.object(fault, "identity", return_value="101"):
                self.assertEqual(fault.signal_status(nonce)["injector_state"], "reused")
            value["injector"]["pid_namespace_inode"] += 1
            path.write_text(json.dumps(value), encoding="utf-8")
            with patch.object(fault, "identity") as identify:
                self.assertEqual(
                    fault.signal_status(nonce)["injector_state"], "namespace_changed"
                )
                self.assertFalse(fault.signal_status(nonce)["injector_quiescent"])
                identify.assert_not_called()

    def test_signal_absence_and_bad_marker_are_not_quiescence(self) -> None:
        nonce = "6" * 32
        with (
            tempfile.TemporaryDirectory() as directory,
            patch.object(fault, "STATE", fault.Path(directory)),
        ):
            self.assertFalse(fault.signal_status(nonce)["injector_quiescent"])
            path = fault.signal_path(nonce)
            marker = self.signal_marker(nonce)
            fault.save_pause(path, path.with_suffix(".json.next"), marker, create=True)
            for key, value in (
                ("phase", "unknown"),
                ("nonce", "wrong"),
                ("version", True),
            ):
                invalid = {**marker, key: value}
                path.write_text(json.dumps(invalid), encoding="utf-8")
                with self.subTest(key=key), self.assertRaises(ValueError):
                    fault.signal_status(nonce)
            path.chmod(0o644)
            with self.assertRaises(ValueError):
                fault.signal_status(nonce)


if __name__ == "__main__":
    unittest.main()
