# /// script
# requires-python = ">=3.13"
# dependencies = []
# ///
"""A receipt is published only after the synchronous transaction completes."""

from __future__ import annotations

import unittest
from unittest.mock import MagicMock, call, patch

import check


class ReceiptTests(unittest.TestCase):
    def test_synchronous_commit_is_set_before_the_single_insert(self) -> None:
        connection = MagicMock()
        cursor = connection.cursor.return_value.__enter__.return_value
        with patch.object(check, "connect", return_value=connection):
            result = check.receipt(42)
        self.assertEqual(
            cursor.execute.call_args_list,
            [
                call("SET LOCAL statement_timeout = '1500ms'"),
                call("SET LOCAL synchronous_commit = 'on'"),
                call(
                    "INSERT INTO lab_receipts(id,payload) VALUES (%s,%s)",
                    (42, "ack-42"),
                ),
            ],
        )
        self.assertTrue(result["acknowledged"])
        self.assertEqual(result["synchronous_commit"], "on")
        connection.__exit__.assert_called_once()

    def test_commit_error_never_publishes_ack_or_retries_insert(self) -> None:
        connection = MagicMock()
        connection.__exit__.side_effect = TimeoutError("COMMIT outcome unknown")
        cursor = connection.cursor.return_value.__enter__.return_value
        with (
            patch.object(check, "connect", return_value=connection),
            self.assertRaises(TimeoutError),
        ):
            check.receipt(42)
        self.assertEqual(
            sum(
                "INSERT INTO" in item.args[0] for item in cursor.execute.call_args_list
            ),
            1,
        )


if __name__ == "__main__":
    unittest.main()
