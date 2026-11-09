//go:build linux

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/ioplane/gopgroute/internal/gobgp"
	"github.com/ioplane/gopgroute/internal/health"
	"github.com/ioplane/gopgroute/internal/pooler"
	"github.com/ioplane/gopgroute/internal/reconcile"
	"github.com/ioplane/gopgroute/internal/supervisor"
	"github.com/osrg/gobgp/v4/api"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func Load(path string) (Config, error) {
	data, err := privateFile(path, configLimit)
	if err != nil {
		return Config{}, err
	}
	return Decode(bytes.NewReader(data))
}

func (cfg Config) socket() string { return filepath.Join(cfg.RuntimeDir, "speaker.sock") }

func (cfg Config) probes() (*health.Patroni, *pooler.Probe, error) {
	password, err := privateFile(cfg.PasswordFile, 4096)
	if err != nil {
		return nil, nil, err
	}
	patroni, err := health.NewPatroni(cfg.PatroniURL, time.Duration(cfg.ProbeTimeout), nil)
	if err != nil {
		return nil, nil, err
	}
	sql, err := pooler.New(pooler.Config{Address: cfg.PoolerAddress, Backend: cfg.Backend, Database: cfg.Database, User: cfg.User, Password: string(password), Timeout: time.Duration(cfg.ProbeTimeout)})
	return patroni, sql, err
}

// Guardian admits one immutable configuration before starting either child.
func Guardian(ctx context.Context, cfg Config, speaker, adapter *exec.Cmd) (result error) {
	if err := cfg.validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if speaker == nil || adapter == nil || speaker == adapter {
		return errors.New("require distinct child commands")
	}
	for _, cmd := range []*exec.Cmd{speaker, adapter} {
		if cmd.Stdin != nil {
			return errors.New("child stdin is reserved for sealed settings")
		}
	}
	for _, dir := range []string{cfg.RuntimeDir, cfg.JournalDir} {
		if err := privateDir(dir); err != nil {
			return err
		}
	}
	if err := cfg.verifySpeaker(); err != nil {
		return err
	}
	if _, _, err := cfg.probes(); err != nil {
		return err
	}
	fd, err := unix.Open(filepath.Join(cfg.RuntimeDir, "guardian.lock"), unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	lock := os.NewFile(uintptr(fd), "guardian-lock")
	defer func() { result = errors.Join(result, lock.Close()) }()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o7777 != 0o600 || int64(stat.Uid) != int64(os.Geteuid()) || stat.Nlink != 1 {
		return errors.New("invalid guardian lock identity")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("lock guardian: %w", err)
	}
	if _, err := os.Lstat(cfg.socket()); !errors.Is(err, os.ErrNotExist) { // #nosec G703 -- Canonical socket path under the admitted service-owned directory.
		return errors.New("runtime socket occupied or inaccessible; recovery required")
	}
	for _, cmd := range []*exec.Cmd{speaker, adapter} {
		settings, err := snapshot(cfg)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, settings.Close()) }()
		cmd.Stdin = settings
	}
	return supervisor.Run(ctx, supervisor.Config{Speaker: speaker, Adapter: adapter, HeartbeatTimeout: time.Duration(cfg.HeartbeatTimeout), StopTimeout: time.Duration(cfg.StopTimeout)})
}

// Speaker replaces only the guardian-owned child with the pinned upstream daemon.
func Speaker(ctx context.Context, cfg Config) (result error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := cfg.validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	executable, err := cfg.openSpeaker()
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, executable.Close()) }()
	settings, err := sealed(speakerConfig(cfg))
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, settings.Close()) }()
	if err := unix.Dup3(int(settings.Fd()), 0, 0); err != nil {
		return err
	}
	unix.Umask(0o077)
	// execve keeps the current Linux thread's PDEATHSIG; Go may have migrated.
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(syscall.SIGKILL), 0, 0, 0); err != nil {
		return err
	}
	if cfg.guardianPID <= 0 || os.Getppid() != cfg.guardianPID {
		return errors.New("guardian identity changed before speaker exec")
	}
	return syscall.Exec("/proc/self/fd/"+strconv.FormatUint(uint64(executable.Fd()), 10), []string{cfg.SpeakerBinary, "--api-hosts", "unix://" + cfg.socket(), "--config-file", "/proc/self/fd/0", "--config-type", "toml", "--pprof-disable", "--metrics-path="}, []string{}) // #nosec G204 -- Execute only the verified root-owned ELF descriptor; no shell or set-id/file capabilities.
}

func Adapter(ctx context.Context, cfg Config, progress *os.File) (result error) {
	if err := cfg.validate(); err != nil {
		return err
	}
	if progress == nil {
		return errors.New("adapter requires progress pipe")
	}
	info, err := progress.Stat()
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		return errors.New("adapter requires inherited pipe")
	}
	patroni, sql, err := cfg.probes()
	if err != nil {
		return err
	}
	startup, cancel := context.WithTimeout(ctx, time.Duration(cfg.StartupTimeout))
	defer cancel()
	if err := waitUntil(startup, func() (bool, error) {
		var stat unix.Stat_t
		err := unix.Lstat(cfg.socket(), &stat)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		mode := stat.Mode & 0o7777
		if stat.Mode&unix.S_IFMT != unix.S_IFSOCK || int64(stat.Uid) != int64(os.Geteuid()) || (mode != 0o700 && mode != 0o600) {
			return false, errors.New("speaker socket identity or mode invalid")
		}
		// Unix sockets start with execute bits under umask 077; the parent is private.
		if mode == 0o700 {
			if err := os.Chmod(cfg.socket(), 0o600); err != nil {
				return false, err
			}
		}
		return true, nil
	}); err != nil {
		return err
	}
	connection, err := grpc.NewClient("unix://"+cfg.socket(), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDisableRetry())
	if err != nil {
		return err
	}
	if err := waitUntil(startup, func() (bool, error) { return speakerReady(startup, api.NewGoBgpServiceClient(connection), cfg) }); err != nil {
		return errors.Join(err, connection.Close())
	}
	if err := connection.Close(); err != nil {
		return err
	}
	routes, err := gobgp.Open(startup, gobgp.Config{Write: cfg.Write, Read: cfg.Read, NextHop: cfg.NextHop, RPCTimeout: time.Duration(cfg.RPCTimeout), MaxAge: time.Duration(cfg.MaxAge)}, cfg.JournalDir, cfg.socket())
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, routes.Close()) }()
	cancel()
	return reconcile.Run(ctx, reconcile.Config{Interval: time.Duration(cfg.Interval), CycleTimeout: time.Duration(cfg.CycleTimeout)}, func(call context.Context) error {
		primary, err := patroni.Probe(call, health.Primary, cfg.MaxLagBytes)
		if err != nil {
			slog.Warn("health observation failed", "probe", "patroni_primary")
		}
		replica, err := patroni.Probe(call, health.Replica, cfg.MaxLagBytes)
		if err != nil {
			slog.Warn("health observation failed", "probe", "patroni_replica")
		}
		now := time.Now()
		intent := health.Decide(now, time.Duration(cfg.MaxAge), health.Snapshot{Primary: primary, Replica: replica, Pooler: health.Check{State: health.Healthy, StartedAt: now}})
		var observation pooler.Observation
		if intent.Write || intent.Read {
			role := health.Primary
			if intent.Read {
				role = health.Replica
			}
			observation, err = sql.Check(call, role)
			if err != nil {
				slog.Warn("health observation failed", "probe", "pooler")
			}
		}
		return routes.Reconcile(call, sqlSnapshot(time.Now(), time.Duration(cfg.MaxAge), primary, replica, observation))
	}, progress)
}

func waitUntil(ctx context.Context, check func() (bool, error)) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ready, err := check()
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
