// Package lab runs the finite isolated Compose failure model, not production admission.
package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

type config struct {
	Transport      []string          `json:"transport"`
	Project        string            `json:"project"`
	Session        string            `json:"session"`
	Images         map[string]string `json:"images"`
	SourceDir      string            `json:"source_dir"`
	Root           string            `json:"root"`
	TimeoutSeconds int               `json:"timeout_seconds"`
}

type probe map[string]any

func (p probe) value(path ...string) (any, error) {
	var v any = map[string]any(p)
	for _, key := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid observation at %s", strings.Join(path, "."))
		}
		v, ok = m[key]
		if !ok {
			return nil, fmt.Errorf("missing observation %s", strings.Join(path, "."))
		}
	}
	return v, nil
}

func (p probe) text(path ...string) (string, error) {
	v, e := p.value(path...)
	if e != nil {
		return "", e
	}
	s, ok := v.(string)
	if !ok {
		return "", errors.New("observation is not a string")
	}
	return s, nil
}

func (p probe) boolean(path ...string) (bool, error) {
	v, e := p.value(path...)
	if e != nil {
		return false, e
	}
	b, ok := v.(bool)
	if !ok {
		return false, errors.New("observation is not a boolean")
	}
	return b, nil
}

func (p probe) number(path ...string) (int, error) {
	v, e := p.value(path...)
	if e != nil {
		return 0, e
	}
	n, ok := v.(float64)
	if !ok || n < 0 || n > 1<<31-1 || n != float64(int(n)) {
		return 0, errors.New("invalid observation integer")
	}
	return int(n), nil
}

func (p probe) list(path ...string) ([]any, error) {
	v, e := p.value(path...)
	if e != nil {
		return nil, e
	}
	a, ok := v.([]any)
	if !ok {
		return nil, errors.New("observation is not a list")
	}
	return a, nil
}

func object(v any) (probe, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("observation is not an object")
	}
	return probe(m), nil
}

type boundedBuffer struct {
	bytes.Buffer
	maximum int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.maximum-b.Len() {
		return 0, errors.New("command output limit exceeded")
	}
	return b.Buffer.Write(p)
}

type commandResult struct {
	Args    []string `json:"args"`
	Exit    int      `json:"exit"`
	Stdout  string   `json:"stdout"`
	Stderr  string   `json:"stderr"`
	Seconds float64  `json:"seconds"`
}

type controller struct {
	cfg       config
	commands  []commandResult
	mu        sync.Mutex
	nonce     string
	uncertain bool
}

func load(path string) (result config, resultErr error) {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0) // #nosec G304 -- Explicit private configuration path; bounded regular-file identity checked before use.
	if e != nil {
		return config{}, e
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	info, e := f.Stat()
	if e != nil {
		return config{}, e
	}
	if !info.Mode().IsRegular() || info.Size() > 65536 || info.Mode().Perm()&0o077 != 0 {
		return config{}, errors.New("private config must be a bounded private regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != int64(os.Geteuid()) || stat.Nlink != 1 {
		return config{}, errors.New("configuration identity must be exclusive and service-owned")
	}
	var c config
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil {
		return c, e
	}
	if e = d.Decode(new(any)); !errors.Is(e, io.EOF) {
		return c, errors.New("trailing configuration data")
	}
	if !regexp.MustCompile(`^agent-[a-z0-9]+-[a-z0-9-]+$`).MatchString(c.Project) || !regexp.MustCompile(`^[a-f0-9-]{36}$`).MatchString(c.Session) {
		return c, errors.New("invalid lab project/session")
	}
	if len(c.Transport) == 0 || !filepath.IsAbs(c.Transport[0]) || !filepath.IsAbs(c.SourceDir) || !filepath.IsAbs(c.Root) || c.TimeoutSeconds < 30 || c.TimeoutSeconds > 180 {
		return c, errors.New("explicit absolute transport/source/root and timeout 30..180 required")
	}
	if len(c.Images) != 10 {
		return c, errors.New("exact ten service image pins required")
	}
	for _, service := range services() {
		if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(c.Images[service]) {
			return c, fmt.Errorf("missing exact image for %s", service)
		}
	}
	return c, nil
}

func services() []string {
	return []string{"leaf1", "leaf2", "leaf3", "db1", "db2", "db3", "route1", "route2", "route3", "client"}
}

func quote(value string) string                  { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
func (c *controller) name(service string) string { return c.cfg.Project + "_" + service + "_1" }

func (c *controller) command(ctx context.Context, duration time.Duration, args ...string) (commandResult, error) {
	call, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = quote(a)
	}
	argv := append(append([]string(nil), c.cfg.Transport[1:]...), strings.Join(quoted, " "))
	cmd := exec.CommandContext(call, c.cfg.Transport[0], argv...) // #nosec G204 -- Explicit private transport, closed laboratory CLI arguments; remote shell only quotes argv.
	cmd.WaitDelay = time.Second
	out, stderr := &boundedBuffer{maximum: 1 << 20}, &boundedBuffer{maximum: 64 << 10}
	cmd.Stdout, cmd.Stderr = out, stderr
	started := time.Now()
	err := cmd.Run()
	r := commandResult{Args: args, Stdout: out.String(), Stderr: stderr.String(), Seconds: time.Since(started).Seconds()}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			r.Exit = exit.ExitCode()
		} else {
			r.Exit = -1
		}
	}
	c.mu.Lock()
	c.commands = append(c.commands, r)
	if err != nil && mutation(args) {
		c.uncertain = true
	}
	c.mu.Unlock()
	if err != nil {
		return r, fmt.Errorf("lab command %s: %w: %s", args[0], err, r.Stderr)
	}
	return r, nil
}

func mutation(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "podman-compose" {
		return true
	}
	if len(args) > 1 {
		switch args[1] {
		case "stop", "start", "restart", "kill", "rm", "pause", "unpause", "update":
			return true
		}
	}
	for i, arg := range args {
		if arg == "/lab/fault.py" && i+1 < len(args) {
			return args[i+1] != "cluster" && args[i+1] != "signal-status"
		}
		if arg == "/lab/check.py" && i+1 < len(args) && args[i+1] == "receipt" {
			return true
		}
		if arg == "configure terminal" {
			return true
		}
		if arg == "tc" {
			for _, v := range args[i+1:] {
				if v == "add" || v == "del" {
					return true
				}
			}
		}
	}
	return false
}

func (c *controller) podman(ctx context.Context, args ...string) (commandResult, error) {
	return c.command(ctx, 15*time.Second, append([]string{"podman"}, args...)...)
}

func (c *controller) execute(ctx context.Context, service string, args ...string) (commandResult, error) {
	return c.podman(ctx, append([]string{"exec", c.name(service)}, args...)...)
}

func (c *controller) readProbe(ctx context.Context, service, mode string) (probe, error) {
	r, e := c.execute(ctx, service, "python3", "/lab/probe.py", mode)
	if e != nil {
		return nil, e
	}
	var p probe
	if e = json.Unmarshal([]byte(r.Stdout), &p); e != nil {
		return nil, e
	}
	v, e := p.number("schema_version")
	if e != nil || v != 1 {
		return nil, errors.New("unsupported observation schema")
	}
	m, e := p.text("mode")
	if e != nil || m != mode {
		return nil, errors.New("observation mode mismatch")
	}
	return p, nil
}

func (c *controller) scope(ctx context.Context) error {
	format := `{"id":"{{.Id}}","name":{{json .Name}},"image":"{{.Image}}","state":"{{.State.Status}}","privileged":{{.HostConfig.Privileged}},"init":{{.HostConfig.Init}},"network":{{json .HostConfig.NetworkMode}},"attachments":{{json .NetworkSettings.Networks}},"ports":{{json .HostConfig.PortBindings}},"labels":{{json .Config.Labels}}}`
	args := []string{"inspect", "--format", format}
	for _, s := range services() {
		args = append(args, c.name(s))
	}
	r, e := c.podman(ctx, args...)
	if e != nil {
		return e
	}
	decoder := json.NewDecoder(strings.NewReader(r.Stdout))
	type meta struct {
		ID, Name, Image, State, Network string
		Privileged, Init                bool
		Ports                           map[string]any
		Attachments                     map[string]any
		Labels                          map[string]string
	}
	rows := map[string]meta{}
	for {
		var m meta
		e = decoder.Decode(&m)
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return e
		}
		if _, ok := rows[m.Name]; ok {
			return errors.New("duplicate scope metadata")
		}
		rows[m.Name] = m
	}
	if len(rows) != 10 {
		return errors.New("incomplete lab scope")
	}
	for _, s := range services() {
		m, ok := rows[c.name(s)]
		if !ok || m.Image != c.cfg.Images[s] || m.State != "running" || m.Privileged || !m.Init || len(m.Ports) != 0 || m.Labels["io.agents-skills.session"] != c.cfg.Session || m.Labels["io.agents-skills.persist"] != "gopgroute-fullstack-lab" {
			return fmt.Errorf("scope drift: %s", s)
		}
		if strings.HasPrefix(s, "route") {
			db := rows[c.name("db"+strings.TrimPrefix(s, "route"))]
			if m.Network != "container:"+db.ID {
				return errors.New("foreign shared network namespace")
			}
		} else if m.Network != "bridge" {
			return errors.New("foreign/host network namespace")
		}
		if !strings.HasPrefix(s, "route") {
			want := []string{"clients"}
			if strings.HasPrefix(s, "db") {
				want = []string{"dc" + strings.TrimPrefix(s, "db")}
			}
			if strings.HasPrefix(s, "leaf") {
				want = []string{"dc" + strings.TrimPrefix(s, "leaf"), "wan"}
				if s == "leaf1" {
					want = append(want, "clients")
				}
			}
			if len(m.Attachments) != len(want) {
				return fmt.Errorf("foreign network attachment: %s", s)
			}
			for _, network := range want {
				if _, ok := m.Attachments[c.cfg.Project+"_"+network]; !ok {
					return fmt.Errorf("missing network attachment: %s", s)
				}
			}
		}
	}
	networkArgs := []string{"network", "inspect", "--format", `{"name":{{json .Name}},"internal":{{.Internal}},"labels":{{json .Labels}}}`}
	for _, name := range []string{"dc1", "dc2", "dc3", "wan", "clients"} {
		networkArgs = append(networkArgs, c.cfg.Project+"_"+name)
	}
	result, e := c.podman(ctx, networkArgs...)
	if e != nil {
		return e
	}
	nd := json.NewDecoder(strings.NewReader(result.Stdout))
	seen := map[string]bool{}
	for {
		var n struct {
			Name     string
			Internal bool
			Labels   map[string]string
		}
		e = nd.Decode(&n)
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return e
		}
		if !n.Internal || seen[n.Name] || n.Labels["io.agents-skills.session"] != c.cfg.Session || n.Labels["io.agents-skills.persist"] != "gopgroute-fullstack-lab" {
			return errors.New("foreign/external lab network")
		}
		seen[n.Name] = true
	}
	if len(seen) != 5 {
		return errors.New("incomplete internal network scope")
	}
	return nil
}

func poll(ctx context.Context, budget time.Duration, check func(context.Context) (bool, error)) error {
	deadline, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	timer := time.NewTicker(300 * time.Millisecond)
	defer timer.Stop()
	var last error
	for {
		if e := deadline.Err(); e != nil {
			return errors.Join(e, last)
		}
		ready, e := check(deadline)
		if ready && e == nil && deadline.Err() == nil {
			return nil
		}
		last = e
		select {
		case <-deadline.Done():
			return errors.Join(deadline.Err(), last)
		case <-timer.C:
		}
	}
}
