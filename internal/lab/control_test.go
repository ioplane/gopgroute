package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFiniteCatalogHasUniqueCompleteCases(t *testing.T) {
	plan := catalog()
	if len(plan) != 48 {
		t.Fatalf("modeled cases=%d", len(plan))
	}
	seen := map[string]bool{}
	for _, sc := range plan {
		if sc.ID == "" || seen[sc.ID] {
			t.Fatalf("duplicate/empty scenario %q", sc.ID)
		}
		seen[sc.ID] = true
		if sc.Kind == "native" && nativeTest(sc.ID) == "" {
			t.Fatal("native case has no executable oracle")
		}
	}
}

func TestPollCannotPassAfterDeadline(t *testing.T) {
	err := poll(t.Context(), time.Millisecond, func(ctx context.Context) (bool, error) { <-ctx.Done(); return true, nil })
	if err == nil {
		t.Fatal("late successful observation bypassed deadline")
	}
}

func privateConfig(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	images := map[string]string{}
	for _, s := range services() {
		images[s] = strings.Repeat("a", 64)
	}
	c := config{Transport: []string{"/missing/transport", "never-executed"}, Project: "agent-deadbeef-lab", Session: "00000000-0000-0000-0000-000000000001", Images: images, SourceDir: root, Root: root, TimeoutSeconds: 90}
	data, e := json.Marshal(c)
	if e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(root, "config.json")
	if e = os.WriteFile(p, data, 0o600); e != nil {
		t.Fatal(e)
	}
	return p
}

func TestDryRunCannotContactTransportOrCreateReport(t *testing.T) {
	path := privateConfig(t)
	target := filepath.Join(t.TempDir(), "must-not-exist")
	var out bytes.Buffer
	if e := Run(t.Context(), []string{"--config", path, "--output", target}, &out); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(target); !os.IsNotExist(e) {
		t.Fatal("dry run mutated output")
	}
	var plan struct {
		DryRun    bool       `json:"dry_run"`
		Scenarios []scenario `json:"scenarios"`
	}
	if e := json.Unmarshal(out.Bytes(), &plan); e != nil || !plan.DryRun || len(plan.Scenarios) != 48 {
		t.Fatalf("invalid dryrun: %v", e)
	}
}

func TestUnknownSelectionRejectsBeforeAnyExecution(t *testing.T) {
	var out bytes.Buffer
	if e := Run(context.Background(), []string{"--config", privateConfig(t), "--only", "nonexistent"}, &out); e == nil {
		t.Fatal("unknown scenario admitted")
	}
}

func TestBoundaryValuesRejectMissingWrongAndFractionalData(t *testing.T) {
	p := probe{"true": true, "negative": float64(-1), "fraction": 0.5, "text": "x"}
	if _, e := p.boolean("missing"); e == nil {
		t.Fatal("missing boolean became false")
	}
	if _, e := p.boolean("text"); e == nil {
		t.Fatal("text became false")
	}
	for _, key := range []string{"negative", "fraction"} {
		if _, e := p.number(key); e == nil {
			t.Fatal("invalid numeric value accepted")
		}
	}
}

func TestCommandOutputLimitIsFailure(t *testing.T) {
	b := boundedBuffer{maximum: 3}
	if _, e := b.Write([]byte("abc")); e != nil {
		t.Fatal(e)
	}
	if _, e := b.Write([]byte("d")); e == nil || b.Len() != 3 {
		t.Fatal("oversize output silently truncated")
	}
}

func TestPreservationIgnoresOnlyActiveJournal(t *testing.T) {
	row := func(hash string) map[string]any { return map[string]any{"name": "retired", "journal_sha256": hash} }
	before := snapshot{Routes: map[string]probe{"1": {"active_generation": "active", "generations": []any{row("before")}}}}
	after := snapshot{Routes: map[string]probe{"1": {"active_generation": "new", "generations": []any{row("changed")}}}}
	if e := preserved(before, after); e == nil {
		t.Fatal("retired journal mutation accepted")
	}
}

func TestReportDoesNotCountLimitsAsSuccessfulTests(t *testing.T) {
	dir := t.TempDir()
	r := report{Project: "isolated", FinalRestored: true, Results: []outcome{{Scenario: scenario{ID: "fencing"}, Verdict: "EXPECTED_LIMITATION", Restored: true}}}
	if e := save(dir, r); e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(filepath.Join(dir, "report.md")) // #nosec G304 -- Exact report path under this test's temporary directory.
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(data), "fencing | PASS") || !strings.Contains(string(data), "## Неуспешно") || !strings.Contains(string(data), "## Ошибки и предупреждения") {
		t.Fatal("report masks missing acceptance")
	}
}

func TestLostReplyReconciliationRequiresEveryIncarnationProof(t *testing.T) {
	makeProbe := func(generation, namespace, started, uuid string) probe {
		var id any = uuid
		if uuid == "" {
			id = nil
		}
		return probe{"active_generation": generation, "pid_namespace": namespace, "processes": map[string]any{"guardian": map[string]any{"start_time": started}}, "generations": []any{map[string]any{"name": generation, "uuid_sha256s": []any{id, nil}}}}
	}
	before := makeProbe("old", "ns1", "1", "uuid1")
	for _, tt := range []struct{ name, gen, ns, start, uuid string }{
		{"same_generation", "old", "ns2", "2", "uuid2"}, {"same_guardian", "new", "ns2", "1", "uuid2"}, {"same_uuid", "new", "ns2", "2", "uuid1"}, {"empty_uuid", "new", "ns2", "2", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if yes, e := restarted(before, makeProbe(tt.gen, tt.ns, tt.start, tt.uuid)); e != nil || yes {
				t.Fatalf("unsafe reconciliation %t %v", yes, e)
			}
		})
	}
	after := makeProbe("new", "ns2", "2", "uuid2")
	delete(after, "active_generation")
	if yes, e := restarted(before, after); e == nil || yes {
		t.Fatal("missing generation accepted")
	}
	if yes, e := restarted(before, makeProbe("new", "ns1", "2", "uuid2")); e != nil || !yes {
		t.Fatalf("complete proof rejected: %v", e)
	}
}

func TestInjectorReconciliationRequiresExactRequestAndQuiescence(t *testing.T) {
	makeStatus := func(pid float64, birth, sig string, quiet bool) probe {
		return probe{"present": true, "target": map[string]any{"pid": pid, "start_time": birth, "signal": sig}, "injector_quiescent": quiet}
	}
	for _, p := range []probe{{"present": false}, makeStatus(2, "1", "SIGKILL", false), makeStatus(3, "1", "SIGKILL", true), makeStatus(2, "2", "SIGKILL", true), makeStatus(2, "1", "SIGSTOP", true)} {
		if ok, _ := quiescentSignal(p, 2, "1", "KILL"); ok {
			t.Fatal("unsafe injector reconciliation")
		}
	}
	if ok, e := quiescentSignal(makeStatus(2, "1", "SIGKILL", true), 2, "1", "KILL"); e != nil || !ok {
		t.Fatalf("complete proof rejected %v", e)
	}
}
