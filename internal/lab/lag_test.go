package lab

import "testing"

func lagSample(write, flush, replay string, lag float64) probe {
	return probe{"postgres": map[string]any{"recovery": false, "postmaster_started": "same", "synchronous_commit": "on", "replication": []any{map[string]any{"pid": float64(42), "application_name": "db3", "state": "streaming", "sync_state": "sync", "write_lsn": write, "flush_lsn": flush, "replay_lsn": replay, "replay_lag_bytes": lag, "flush_lag_bytes": float64(0)}}}}
}

func TestLagProofRequiresStableReplayDurableProgressAndSameSyncStream(t *testing.T) {
	before := lagSample("0/100", "0/100", "0/100", 0)
	after := lagSample("0/200200", "0/200200", "0/100", 2097408)
	if ok, e := lagConfirmed(before, after, 3); e != nil || !ok {
		t.Fatalf("valid replay-only lag rejected: %v", e)
	}
	for _, key := range []string{"pid", "sync_state", "replay_lsn", "flush_lsn", "flush_lag_bytes", "state"} {
		t.Run(key, func(t *testing.T) {
			bad := lagSample("0/200200", "0/200200", "0/100", 2097408)
			row := bad["postgres"].(map[string]any)["replication"].([]any)[0].(map[string]any)
			row[key] = map[string]any{"pid": float64(43), "sync_state": "async", "replay_lsn": "0/101", "flush_lsn": "0/100", "flush_lag_bytes": float64(100), "state": "catchup"}[key]
			if ok, _ := lagConfirmed(before, bad, 3); ok {
				t.Fatal("unproven lag accepted")
			}
		})
	}
}

func TestWALPositionIsStrictAndHandlesMoreThanTwoGiB(t *testing.T) {
	for _, invalid := range []string{"", "1", "/1", "1/", "1/2/3", "100000000/0", "0/-1", "garbage/0"} {
		if _, e := walPosition(invalid); e == nil {
			t.Fatalf("invalid LSN accepted: %q", invalid)
		}
	}
	a, e := walPosition("0/FFFFFFFF")
	if e != nil {
		t.Fatal(e)
	}
	b, e := walPosition("1/0")
	if e != nil || b != a+1 {
		t.Fatalf("WAL high word lost: %d %d %v", a, b, e)
	}
}

func TestStartupStopRequiresTheOriginalProcessTreeAndNamespace(t *testing.T) {
	before := probe{"pid_namespace": "same", "process_ids": map[string]any{"startup": float64(42), "postgres": float64(10)}, "process_start_ids": map[string]any{"startup": "200", "postgres": "100"}}
	for _, key := range []string{"pid", "start_time", "postmaster_pid", "postmaster_start_time", "pid_namespace", "state", "valid"} {
		t.Run(key, func(t *testing.T) {
			p := probe{"pid": float64(42), "start_time": "200", "postmaster_pid": float64(10), "postmaster_start_time": "100", "pid_namespace": "same", "state": "T"}
			if key != "valid" {
				p[key] = map[string]any{"pid": float64(43), "start_time": "201", "postmaster_pid": float64(11), "postmaster_start_time": "101", "pid_namespace": "other", "state": "S"}[key]
			}
			ok, e := startupStopped(p, before)
			if key == "valid" && (e != nil || !ok) || key != "valid" && ok {
				t.Fatalf("startup proof: %t %v", ok, e)
			}
		})
	}
}
