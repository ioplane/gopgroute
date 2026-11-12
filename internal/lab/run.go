package lab

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type report struct {
	Schema        int             `json:"schema_version"`
	Started       string          `json:"started_utc"`
	Finished      string          `json:"finished_utc"`
	Project       string          `json:"project"`
	Nonce         string          `json:"campaign_nonce"`
	Plan          []scenario      `json:"plan"`
	Commands      []commandResult `json:"execution_log"`
	Results       []outcome       `json:"results"`
	Acknowledged  []int           `json:"acknowledged_ids"`
	Unknown       []int           `json:"unknown_commit_ids"`
	FinalRestored bool            `json:"final_restored"`
	NativeCommand *commandResult  `json:"native_command,omitempty"`
	Fatal         string          `json:"fatal,omitempty"`
	Limitations   []string        `json:"limitations"`
}

// Run is dry-run by default. Apply requires an exact private lab identity file.
func Run(ctx context.Context, args []string, stdout io.Writer) (result error) {
	flags := flag.NewFlagSet("gopgroute-lab", flag.ContinueOnError)
	path := flags.String("config", "", "private JSON lab identity")
	output := flags.String("output", "", "new private report directory (required with apply)")
	apply := flags.Bool("apply", false, "execute the closed failure model")
	only := flags.String("only", "", "comma-separated scenario IDs; empty selects the complete model")
	if e := flags.Parse(args); e != nil {
		return e
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	cfg, e := load(*path)
	if e != nil {
		return e
	}
	plan := catalog()
	if *only != "" {
		plan = nil
		wanted := strings.Split(*only, ",")
		valid := map[string]scenario{}
		for _, sc := range catalog() {
			valid[sc.ID] = sc
		}
		for _, id := range wanted {
			sc, ok := valid[id]
			if !ok {
				return fmt.Errorf("unknown scenario %s", id)
			}
			plan = append(plan, sc)
		}
	}
	if !*apply {
		return json.NewEncoder(stdout).Encode(struct {
			DryRun    bool       `json:"dry_run"`
			Scenarios []scenario `json:"scenarios"`
		}{true, plan})
	}
	if !filepath.IsAbs(*output) {
		return errors.New("absolute new output directory required")
	}
	if e = os.Mkdir(*output, 0o700); e != nil {
		return e
	}
	r := report{Schema: 1, Started: time.Now().UTC().Format(time.RFC3339Nano), Project: cfg.Project, Limitations: []string{"Finite declared fault model; not every combination or physical failure.", "Single physical host; hardware watchdog off; no host reboot, power, media fault or PITR qualification.", "Leaf1 is the only client ingress; BGP edge tests are session withdrawal, not a cable fault.", "Lost commit response is UNKNOWN and stops the campaign; no INSERT replay.", "Timing includes controller/SSH/probe overhead; no production RTO or universal RPO guarantee."}}
	c := &controller{cfg: cfg}
	r.Plan = plan
	defer func() {
		r.Commands = append([]commandResult(nil), c.commands...)
		if result != nil {
			seen := map[string]bool{}
			for _, v := range r.Results {
				seen[v.Scenario.ID] = true
			}
			for _, sc := range plan {
				if !seen[sc.ID] {
					r.Results = append(r.Results, outcome{Scenario: sc, Verdict: "BLOCKED", Error: "Campaign stopped before this scenario"})
				}
			}
		}
		r.Finished = time.Now().UTC().Format(time.RFC3339Nano)
		if result != nil {
			r.Fatal = result.Error()
		}
		result = errors.Join(result, save(*output, r))
	}()
	if e = c.scope(ctx); e != nil {
		return e
	}
	var token [16]byte
	if _, e = rand.Read(token[:]); e != nil {
		return e
	}
	nonce := fmt.Sprintf("%x", token[:])
	c.nonce = nonce
	r.Nonce = nonce
	if _, e = c.execute(ctx, "client", "python3", "/lab/fault.py", "campaign-acquire", nonce); e != nil {
		return e
	}
	defer func() {
		if !r.FinalRestored || c.uncertain {
			r.FinalRestored = false
			return
		} // Retain the marker when recovery is unproven.
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if e := c.scope(cleanup); e != nil {
			r.FinalRestored = false
			result = errors.Join(result, e)
			return
		}
		if _, e := c.restored(cleanup); e != nil {
			r.FinalRestored = false
			result = errors.Join(result, e)
			return
		}
		if e := c.verify(cleanup, r.Acknowledged); e != nil {
			r.FinalRestored = false
			result = errors.Join(result, e)
			return
		}
		_, e := c.execute(cleanup, "client", "python3", "/lab/fault.py", "campaign-release", nonce)
		result = errors.Join(result, e)
	}()
	baseline, e := c.restored(ctx)
	if e != nil {
		return e
	}
	r.FinalRestored = true
	ids, e := c.ledger(ctx)
	if e != nil {
		return e
	}
	r.Acknowledged = ids
	var native map[string]bool
	for _, sc := range plan {
		if e = ctx.Err(); e != nil {
			return e
		}
		if sc.Kind == "native" {
			if native == nil {
				var command commandResult
				native, command, e = nativeSuite(ctx, cfg.SourceDir)
				r.NativeCommand = &command
				if e != nil {
					return e
				}
			}
			name := nativeTest(sc.ID)
			passed := native[name]
			verdict := "PASS"
			failure := ""
			if !passed {
				verdict = "FAIL"
				failure = "native test did not emit an individual PASS: " + name
			}
			r.Results = append(r.Results, outcome{Scenario: sc, Verdict: verdict, Restored: true, Error: failure})
			if e = save(*output, r); e != nil {
				return e
			}
			if _, e = fmt.Fprintf(stdout, "%s %s\n", sc.ID, verdict); e != nil {
				return e
			}
			continue
		}
		// Reset only a route container's bounded automatic-retry budget, retaining both data roots.
		r.FinalRestored = false
		if e = c.scope(ctx); e != nil {
			return e
		}
		if sc.Kind == "route" || sc.Kind == "paired" {
			p, replicas, e := baseline.roles()
			if e != nil {
				return e
			}
			node := p
			if sc.Role == "replica" {
				node = replicas[0]
			}
			if sc.Role == "replica2" {
				node = replicas[1]
			}
			if e = c.refreshRoute(ctx, node); e != nil {
				return e
			}
		}
		if sc.Kind == "switch" {
			p, _, e := baseline.roles()
			if e != nil {
				return e
			}
			if p == sc.Node {
				if e = c.switchTo(ctx, p%3+1); e != nil {
					return e
				}
			}
		}
		if sc.Kind == "leaf" && sc.Node > 1 {
			// Exercise loss of the site's primary, rather than whichever replica happens to be there.
			if e = c.switchTo(ctx, sc.Node); e != nil {
				return e
			}
		}
		baseline, e = c.restored(ctx)
		if e != nil {
			return e
		}
		primary, _, e := baseline.roles()
		if e != nil {
			return e
		}
		c.mu.Lock()
		uncertain := c.uncertain
		c.mu.Unlock()
		if e = preparationReady(sc, primary, uncertain); e != nil {
			return e
		}
		if e = c.verify(ctx, r.Acknowledged); e != nil {
			return e
		}
		if _, e = c.receipt(ctx, &r); e != nil {
			return e
		}
		r.Results = append(r.Results, outcome{Scenario: sc, Verdict: "IN_PROGRESS", Before: &baseline})
		r.Commands = append([]commandResult(nil), c.commands...)
		if e = save(*output, r); e != nil {
			return e
		}
		v := c.caseRun(ctx, sc, baseline, func(ctx context.Context) (probe, error) { return c.receipt(ctx, &r) })
		if c.uncertain {
			v.Restored = false
			v.Verdict = "ERROR"
			v.Error = errors.Join(errors.New(v.Error), errors.New("mutating command failed; retained campaign marker requires review before another fault")).Error()
		}
		r.Results[len(r.Results)-1] = v
		if v.Restored {
			if e = c.verify(ctx, r.Acknowledged); e != nil {
				r.Results[len(r.Results)-1].Verdict = "FAIL"
				r.Results[len(r.Results)-1].Error = e.Error()
				v.Restored = false
				r.Results[len(r.Results)-1].Restored = false
			}
		}
		r.FinalRestored = v.Restored
		r.Commands = append([]commandResult(nil), c.commands...)
		if e = save(*output, r); e != nil {
			return e
		}
		if _, e = fmt.Fprintf(stdout, "%s %s restored=%t\n", sc.ID, r.Results[len(r.Results)-1].Verdict, v.Restored); e != nil {
			return e
		}
		if !v.Restored {
			return errors.New("recovery or transaction verification failed; remaining scenarios BLOCKED")
		}
		baseline = *v.After
	}
	r.FinalRestored = false
	if _, e = c.restored(ctx); e != nil {
		return e
	}
	if e = c.verify(ctx, r.Acknowledged); e != nil {
		return e
	}
	r.FinalRestored = true
	for _, v := range r.Results {
		if v.Verdict == "FAIL" || v.Verdict == "ERROR" || v.Verdict == "RECOVERY_FAILED" {
			return errors.New("one or more modeled invariants failed; inspect report")
		}
	}
	return nil
}

func (c *controller) receipt(ctx context.Context, r *report) (probe, error) {
	id := 1
	for _, n := range r.Acknowledged {
		if n >= id {
			id = n + 1
		}
	}
	// Synchronous-standby replacement can outlast the generic 15-second observation transport.
	v, e := c.command(ctx, 45*time.Second, "podman", "exec", c.name("client"), "python3", "/lab/check.py", "receipt", strconv.Itoa(id))
	var p probe
	if e == nil {
		e = json.Unmarshal([]byte(v.Stdout), &p)
	}
	if e == nil {
		got, err := p.number("id")
		ack, err2 := p.boolean("acknowledged")
		payload, err3 := p.text("payload")
		if err != nil || err2 != nil || err3 != nil || got != id || !ack || payload != fmt.Sprintf("ack-%d", id) {
			e = errors.New("invalid COMMIT acknowledgement")
		}
	}
	if e != nil {
		r.Unknown = append(r.Unknown, id)
		c.mu.Lock()
		c.uncertain = true
		c.mu.Unlock()
		return p, errors.Join(errors.New("UNKNOWN COMMIT outcome; campaign stopped without replay"), e)
	}
	r.Acknowledged = append(r.Acknowledged, id)
	return p, nil
}

func (c *controller) ledger(ctx context.Context) ([]int, error) {
	p, e := c.readProbe(ctx, "client", "ledger")
	if e != nil {
		return nil, e
	}
	rows, e := p.list("rows")
	if e != nil {
		return nil, e
	}
	var ids []int
	for _, v := range rows {
		row, ok := v.([]any)
		if !ok || len(row) != 2 {
			return nil, errors.New("invalid ledger row")
		}
		n, ok := row[0].(float64)
		if !ok || n <= 0 || n > 1<<31-1 || n != float64(int(n)) || row[1] != fmt.Sprintf("ack-%d", int(n)) {
			return nil, errors.New("foreign or malformed ledger entry")
		}
		ids = append(ids, int(n))
	}
	return ids, nil
}

func (c *controller) verify(ctx context.Context, ids []int) error {
	args := []string{"python3", "/lab/check.py", "verify"}
	for _, n := range ids {
		args = append(args, strconv.Itoa(n))
	}
	_, e := c.execute(ctx, "client", args...)
	return e
}

func nativeTest(id string) string {
	switch id {
	case "native-second-guardian":
		return "TestGuardianSameRootsExcludesConcurrentStart"
	case "native-runtime-root-change", "native-journal-root-change":
		return "TestEitherLeaseExcludesChangedOtherRoot"
	case "native-speaker-lease-exec":
		return "TestSpeakerExecRetainsLeasesAfterGuardianCopiesClose"
	case "native-adapter-lease-inheritance":
		return "TestGuardianDeathKillsExecSpeakerAndAdapter"
	case "native-legacy-root-refusal":
		return "TestGuardianRejectsLegacyAndUnknownRoots"
	case "native-uncertain-generation-retirement":
		return "TestIncarnationsPreserveUncertainOldJournal"
	}
	return ""
}

func nativeSuite(ctx context.Context, dir string) (map[string]bool, commandResult, error) {
	args := []string{"test", "-json", "-race", "-shuffle=on", "-count=1", "-timeout=120s", "./internal/app", "./internal/gobgp", "./internal/health", "./internal/pooler", "./internal/supervisor"}
	call, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(call, "/usr/local/go/bin/go", args...)
	cmd.Dir = dir
	out, stderr := &boundedBuffer{maximum: 1 << 20}, &boundedBuffer{maximum: 64 << 10}
	cmd.Stdout, cmd.Stderr = out, stderr
	t0 := time.Now()
	e := cmd.Run()
	r := commandResult{Args: append([]string{"/usr/local/go/bin/go"}, args...), Stdout: out.String(), Stderr: stderr.String(), Seconds: time.Since(t0).Seconds()}
	if e != nil {
		r.Exit = 1
		return nil, r, e
	}
	passed := map[string]bool{}
	decoder := json.NewDecoder(strings.NewReader(r.Stdout))
	for {
		var event struct{ Action, Test string }
		e = decoder.Decode(&event)
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return nil, r, e
		}
		if event.Action == "pass" && event.Test != "" {
			passed[event.Test] = true
		}
	}
	if len(passed) == 0 {
		return nil, r, errors.New("native suite passed no tests")
	}
	return passed, r, nil
}

func save(dir string, r report) error {
	data, e := json.MarshalIndent(r, "", "  ")
	if e != nil {
		return e
	}
	p := filepath.Join(dir, "report.json.tmp")
	if e = os.WriteFile(p, append(data, '\n'), 0o600); e != nil {
		return e
	}
	if e = os.Rename(p, filepath.Join(dir, "report.json")); e != nil {
		return e
	}
	var b strings.Builder
	status := "выполнено"
	if r.Fatal != "" || !r.FinalRestored {
		status = "выполнено частично"
	}
	executed, blocked := 0, 0
	for _, v := range r.Results {
		if v.Verdict == "BLOCKED" || v.Verdict == "IN_PROGRESS" {
			blocked++
		} else {
			executed++
		}
	}
	fmt.Fprintf(&b, "**Итог:** %s; исполнено %d сценариев конечной модели, не исполнено %d.\n**От вас:** не требуется\n\n## Сделано\n\n- Выполнена кампания `%s`; доказательства сохранены в `report.json`.\n\n## Успешно\n\n| Проверка | Результат | Доказательство |\n|---|---|---|\n", status, executed, blocked, r.Project)
	for _, v := range r.Results {
		if v.Verdict == "PASS" {
			fmt.Fprintf(&b, "| %s | PASS, restored=%t | report.json |\n", v.Scenario.ID, v.Restored)
		}
	}
	b.WriteString("\n## Неуспешно\n\n")
	failed := false
	failures := 0
	for _, v := range r.Results {
		if v.Verdict != "PASS" && v.Verdict != "EXPECTED_LIMITATION" {
			if !failed {
				b.WriteString("| № | Что | Причина | Статус | Дальше |\n|---|---|---|---|---|\n")
			}
			failed = true
			failures++
			fmt.Fprintf(&b, "| Н%d | %s | %s | открыто | Проверить соответствующий scenario в report.json |\n", failures, v.Scenario.ID, v.Verdict)
		}
	}
	if !failed {
		b.WriteString("нет\n")
	}
	b.WriteString("\n## Ошибки и предупреждения\n\n| № | Источник | Суть | Статус | Дальше |\n|---|---|---|---|---|\n")
	n := 0
	for _, v := range r.Results {
		if v.Verdict == "EXPECTED_LIMITATION" {
			n++
			fmt.Fprintf(&b, "| О%d | %s | Граница лабораторного профиля | принято | См. измеренные условия в report.json |\n", n, v.Scenario.ID)
		}
	}
	fmt.Fprintf(&b, "| О%d | Приёмка | Single-host, watchdog off; production не подтверждён | принято | Отдельная приёмка fencing и PITR |\n", n+1)
	if r.Fatal != "" {
		fmt.Fprintf(&b, "\n```text\nFATAL %s\n```\n", r.Fatal)
	}
	return os.WriteFile(filepath.Join(dir, "report.md"), []byte(b.String()), 0o600)
}
