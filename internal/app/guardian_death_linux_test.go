//go:build linux

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestCLIParentHelper(t *testing.T) {
	if os.Getenv("GOPGROUTE_CLI_PARENT") != "1" {
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if err := Run(ctx, []string{"-config", os.Args[len(os.Args)-1]}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestGuardianDeathKillsExecSpeakerAndAdapter(t *testing.T) {
	cfg := testConfig(t)
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(cfg.RuntimeDir), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	guardian := exec.Command(os.Args[0], "-test.run=^TestCLIParentHelper$", "--", path) // #nosec G204 G702 -- Relaunch only this test's CLI guardian.
	guardian.Env = append(os.Environ(), "GOPGROUTE_CLI_PARENT=1", "GORACE=atexit_sleep_ms=0")
	guardian.Stderr = os.Stderr
	if err := guardian.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if guardian.ProcessState != nil {
			return
		}
		if err := guardian.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		if err := guardian.Wait(); err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Error(err)
			}
		}
	})
	connection, err := grpc.NewClient("unix://"+liveSocket(t, cfg), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDisableRetry())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := connection.Close(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := waitUntil(ctx, func() (bool, error) { return speakerReady(ctx, api.NewGoBgpServiceClient(connection), cfg) }); err != nil {
		t.Fatal(err)
	}
	root := fmt.Sprintf("/proc/%d/task", guardian.Process.Pid)
	tasks, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var handles []int
	for _, task := range tasks {
		data, err := os.ReadFile(filepath.Join(root, task.Name(), "children")) // #nosec G304 -- Kernel task entries of the exact child guardian started by this test.
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(text)
			if err != nil || pid <= 0 {
				t.Fatal("invalid owned child PID")
			}
			// Both direct children must retain both leases, including after speaker exec.
			for _, dir := range []string{cfg.RuntimeDir, cfg.JournalDir} {
				lease, err := os.Stat(filepath.Join(dir, "guardian.lock"))
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, descriptor := range []string{"3", "4", "5"} {
					inherited, err := os.Stat(fmt.Sprintf("/proc/%d/fd/%s", pid, descriptor))
					if err == nil && os.SameFile(lease, inherited) {
						found = true
					}
				}
				if !found {
					t.Fatal("supervised child missing inherited incarnation lease")
				}
			}
			fd, err := unix.PidfdOpen(pid, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := unix.Close(fd); err != nil {
					t.Error(err)
				}
			})
			handles = append(handles, fd)
		}
	}
	if len(handles) != 2 {
		t.Fatalf("expected two owned children, got %d", len(handles))
	}
	if err := guardian.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := guardian.Wait(); err == nil {
		t.Fatal("guardian survived SIGKILL")
	}
	for _, fd := range handles {
		if fd < 0 || fd > 1<<31-1 {
			t.Fatal("invalid pidfd range")
		}
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}} // #nosec G115 -- Checked range; stable pidfd authority, not a reusable PID.
		if n, err := unix.Poll(poll, 2000); err != nil || n != 1 || poll[0].Revents&unix.POLLIN == 0 {
			t.Fatalf("exec child survived guardian death: n=%d err=%v", n, err)
		}
	}
	oldSocket := strings.TrimPrefix(connection.Target(), "unix://")
	guardian = exec.Command(os.Args[0], "-test.run=^TestCLIParentHelper$", "--", path) // #nosec G204 G702 -- Restart the same test guardian with unchanged configuration.
	guardian.Env = append(os.Environ(), "GOPGROUTE_CLI_PARENT=1", "GORACE=atexit_sleep_ms=0")
	guardian.Stderr = os.Stderr
	if err := guardian.Start(); err != nil {
		t.Fatal(err)
	}
	newSocket := liveSocket(t, cfg)
	if newSocket == oldSocket {
		t.Fatal("SIGKILL restart reused speaker socket")
	}
	second, err := grpc.NewClient("unix://"+newSocket, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDisableRetry())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := second.Close(); err != nil {
			t.Error(err)
		}
	}()
	restarted, finish := context.WithTimeout(t.Context(), 3*time.Second)
	defer finish()
	if err := waitUntil(restarted, func() (bool, error) { return speakerReady(restarted, api.NewGoBgpServiceClient(second), cfg) }); err != nil {
		t.Fatal(err)
	}
}
