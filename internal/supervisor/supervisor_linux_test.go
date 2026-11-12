//go:build linux

package supervisor

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The real child protocol makes a missing stop, pipe close or heartbeat expiry
// observable without mocking the supervisor's own process interactions.
func TestProcessHelper(t *testing.T) {
	mode := os.Getenv("GOPGROUTE_SUPERVISOR_HELPER")
	if mode == "" {
		return
	}
	if mode == "guardian" {
		speaker := helperCommand("speaker")
		adapter := helperCommand("adapter")
		speaker.Stdin, adapter.Stdin = os.Stdin, os.Stdin
		speaker.Stdout, adapter.Stdout = os.Stdout, os.Stdout
		if err := Run(context.Background(), Config{
			Speaker: speaker, Adapter: adapter,
			HeartbeatTimeout: 10 * time.Second, StopTimeout: 100 * time.Millisecond,
		}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(8)
		}
		os.Exit(0)
	}
	if mode == "ignore-term" {
		signal.Ignore(syscall.SIGTERM)
	}
	if err := binary.Write(os.Stdout, binary.LittleEndian, int64(os.Getpid())); err != nil {
		os.Exit(9)
	}
	heartbeat := os.NewFile(3, "heartbeat")
	var command [1]byte
	for {
		if _, err := io.ReadFull(os.Stdin, command[:]); err != nil {
			os.Exit(0)
		}
		switch command[0] {
		case '+', '?':
			if _, err := heartbeat.Write(command[:]); err != nil {
				os.Exit(10)
			}
		case 'e':
			if err := heartbeat.Close(); err != nil {
				os.Exit(11)
			}
		case 'x':
			os.Exit(0)
		case 'k':
			if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
				os.Exit(12)
			}
		case 's':
			if err := syscall.Kill(os.Getpid(), syscall.SIGSTOP); err != nil {
				os.Exit(13)
			}
		}
	}
}

func TestRejectsInvalidConfigurationBeforeStartingChildren(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*Config)
	}{
		{"missing speaker", func(c *Config) { c.Speaker = nil }},
		{"missing adapter", func(c *Config) { c.Adapter = nil }},
		{"same command", func(c *Config) { c.Adapter = c.Speaker }},
		{"heartbeat deadline", func(c *Config) { c.HeartbeatTimeout = 0 }},
		{"stop deadline", func(c *Config) { c.StopTimeout = 0 }},
		{"inherited fd", func(c *Config) { c.Adapter.ExtraFiles = []*os.File{os.Stdin} }},
		{"process attributes", func(c *Config) { c.Speaker.SysProcAttr = &syscall.SysProcAttr{} }},
		{"copying stdin", func(c *Config) { c.Speaker.Stdin = strings.NewReader("input") }},
		{"copying stdout", func(c *Config) { c.Speaker.Stdout = &bytes.Buffer{} }},
		{"copying stderr", func(c *Config) { c.Adapter.Stderr = &bytes.Buffer{} }},
		{"custom cancel", func(c *Config) { c.Adapter.Cancel = func() error { return nil } }},
		{"already started", func(c *Config) { c.Adapter.Process = &os.Process{} }},
		{"typed nil stream", func(c *Config) { c.Adapter.Stdout = (*os.File)(nil) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			speaker, adapter := helperCommand("speaker"), helperCommand("adapter")
			cfg := Config{Speaker: speaker, Adapter: adapter, HeartbeatTimeout: time.Second, StopTimeout: time.Second}
			test.edit(&cfg)
			speakerProcess, adapterProcess := speaker.Process, adapter.Process
			if err := Run(t.Context(), cfg); err == nil {
				t.Fatal("invalid configuration accepted")
			}
			if speaker.Process != speakerProcess || adapter.Process != adapterProcess {
				t.Fatal("configuration rejection started a child")
			}
		})
	}
}

func TestCancellationJoinsBothOwnedChildren(t *testing.T) {
	h := startSupervisor(t, "adapter", time.Second)
	h.cancel()
	if err := h.result(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	h.assertReaped(t)
}

func TestAdapterFailuresStopSpeaker(t *testing.T) {
	for _, test := range []struct{ name, command, cause string }{
		{"clean exit", "x", ""},
		{"SIGKILL", "k", ""},
		{"SIGSTOP", "s", "heartbeat timeout"},
		{"malformed heartbeat", "?", "invalid adapter heartbeat"},
		{"heartbeat EOF", "e", "read adapter heartbeat"},
		{"heartbeat stall", "", "heartbeat timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := startSupervisor(t, "adapter", 200*time.Millisecond)
			if test.command != "" {
				h.send(t, h.adapterInput, test.command)
			}
			if err := h.result(t); err == nil || !strings.Contains(err.Error(), test.cause) {
				t.Fatalf("adapter failure: %v; want cause %q", err, test.cause)
			}
			h.assertReaped(t)
		})
	}
}

func TestSpeakerExitEndsAdapterIncarnation(t *testing.T) {
	h := startSupervisor(t, "adapter", time.Second)
	h.send(t, h.speakerInput, "x")
	if err := h.result(t); err == nil || !strings.Contains(err.Error(), "speaker") {
		t.Fatalf("speaker exit: %v", err)
	}
	h.assertReaped(t)
}

func TestHeartbeatsKeepChildrenAliveUntilCancellation(t *testing.T) {
	h := startSupervisor(t, "adapter", 400*time.Millisecond)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for range 6 {
		select {
		case err := <-h.done:
			t.Fatalf("stopped while receiving heartbeats: %v", err)
		case <-ticker.C:
			h.send(t, h.adapterInput, "+")
		}
	}
	h.cancel()
	if err := h.result(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	h.assertReaped(t)
}

func TestSIGTERMIgnoringChildIsKilled(t *testing.T) {
	h := startSupervisor(t, "ignore-term", time.Second)
	h.cancel()
	if err := h.result(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	h.assertReaped(t)
	if h.cfg.Adapter.ProcessState.Success() {
		t.Fatal("SIGTERM-ignoring adapter was not killed")
	}
}

func TestAdapterStartFailureStopsStartedSpeaker(t *testing.T) {
	speaker := helperCommand("speaker")
	adapter := exec.Command("/missing/gopgroute-test-adapter")
	err := Run(t.Context(), Config{Speaker: speaker, Adapter: adapter, HeartbeatTimeout: time.Second, StopTimeout: 100 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "adapter") {
		t.Fatalf("adapter start: %v", err)
	}
	if speaker.Process == nil || speaker.ProcessState == nil {
		t.Fatal("started speaker was not joined")
	}
}

func TestSpeakerStartFailureDoesNotStartAdapter(t *testing.T) {
	adapter := helperCommand("adapter")
	err := Run(t.Context(), Config{Speaker: exec.Command("/missing/gopgroute-test-speaker"), Adapter: adapter, HeartbeatTimeout: time.Second, StopTimeout: time.Second})
	if err == nil || adapter.Process != nil {
		t.Fatalf("speaker start: err=%v adapter=%v", err, adapter.Process)
	}
}

func TestCanceledContextDoesNotStartChildren(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	speaker, adapter := helperCommand("speaker"), helperCommand("adapter")
	err := Run(ctx, Config{Speaker: speaker, Adapter: adapter, HeartbeatTimeout: time.Second, StopTimeout: time.Second})
	if !errors.Is(err, context.Canceled) || speaker.Process != nil || adapter.Process != nil {
		t.Fatalf("pre-canceled context: %v", err)
	}
}

func TestExpiredHeartbeatCannotReviveLease(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := read.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := write.Write([]byte{'+'}); err != nil {
		t.Fatal(err)
	}
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	last := time.Now().Add(-2 * time.Second)
	progress := &heartbeatProgress{last: last}
	err = readHeartbeats(read, progress, time.Second)
	if err == nil || !strings.Contains(err.Error(), "heartbeat timeout") || progress.last != last {
		t.Fatalf("expired heartbeat renewed lease: err=%v last=%v want=%v", err, progress.last, last)
	}
}

func TestGuardianSIGKILLStopsItsChildrenAndPreservesUnrelatedProcess(t *testing.T) {
	input, inputWrite := testPipe(t)
	outputRead, output := testPipe(t)
	guardian := helperCommand("guardian")
	guardian.Stdin, guardian.Stdout, guardian.Stderr = input, output, os.Stderr
	if err := guardian.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopHelper(t, guardian) })
	_ = inputWrite // Keep the owned children blocked on their control input.
	children := []int{readPID(t, outputRead), readPID(t, outputRead)}
	pidfds := make([]unix.PollFd, 0, len(children))
	for _, pid := range children {
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			t.Fatalf("pidfd for child %d: %v", pid, err)
		}
		t.Cleanup(func() { closeFD(t, fd) })
		if fd < 0 || fd > 1<<31-1 {
			t.Fatalf("invalid pidfd: %d", fd)
		}
		pidfds = append(pidfds, unix.PollFd{Fd: int32(fd), Events: unix.POLLIN}) // #nosec G115 -- Range checked immediately above.
	}
	unrelated := helperCommand("speaker")
	unrelated.Stdin = input
	unrelatedRead, unrelatedWrite := testPipe(t)
	unrelated.Stdout = unrelatedWrite
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopHelper(t, unrelated) })
	readPID(t, unrelatedRead)
	if err := guardian.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := guardian.Wait(); err == nil {
		t.Fatal("SIGKILL returned success")
	}
	for _, fd := range pidfds {
		poll := []unix.PollFd{fd}
		if n, err := unix.Poll(poll, 2000); err != nil || n != 1 || poll[0].Revents&unix.POLLIN == 0 {
			t.Fatalf("guardian left owned child alive: n=%d err=%v events=%v", n, err, poll[0].Revents)
		}
	}
	if err := unrelated.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("unrelated process affected: %v", err)
	}
}

type supervisorHarness struct {
	cfg                        Config
	cancel                     context.CancelFunc
	done                       chan error
	speakerInput, adapterInput *os.File
}

func startSupervisor(t *testing.T, adapterMode string, timeout time.Duration) *supervisorHarness {
	t.Helper()
	speakerRead, speakerInput := testPipe(t)
	adapterRead, adapterInput := testPipe(t)
	speakerOutput, speakerWrite := testPipe(t)
	adapterOutput, adapterWrite := testPipe(t)
	speaker, adapter := helperCommand("speaker"), helperCommand(adapterMode)
	speaker.Stdin, speaker.Stdout, speaker.Stderr = speakerRead, speakerWrite, os.Stderr
	adapter.Stdin, adapter.Stdout, adapter.Stderr = adapterRead, adapterWrite, os.Stderr
	ctx, cancel := context.WithCancel(t.Context())
	h := &supervisorHarness{
		cfg:    Config{Speaker: speaker, Adapter: adapter, HeartbeatTimeout: timeout, StopTimeout: 100 * time.Millisecond},
		cancel: cancel, done: make(chan error, 1), speakerInput: speakerInput, adapterInput: adapterInput,
	}
	go func() {
		h.done <- Run(ctx, h.cfg)
		close(h.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-h.done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Logf("supervisor cleanup: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("supervisor cleanup did not complete")
		}
	})
	readPID(t, speakerOutput)
	readPID(t, adapterOutput)
	h.send(t, adapterInput, "+")
	return h
}

func (h *supervisorHarness) send(t *testing.T, pipe *os.File, command string) {
	t.Helper()
	if _, err := io.WriteString(pipe, command); err != nil {
		t.Fatal(err)
	}
}

func (h *supervisorHarness) result(t *testing.T) error {
	t.Helper()
	select {
	case err := <-h.done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not return within cleanup bound")
		return nil
	}
}

func (h *supervisorHarness) assertReaped(t *testing.T) {
	t.Helper()
	for _, cmd := range []*exec.Cmd{h.cfg.Speaker, h.cfg.Adapter} {
		if cmd.Process == nil || cmd.ProcessState == nil {
			t.Fatal("owned child was not joined")
		}
		if err := cmd.Process.Signal(syscall.Signal(0)); !errors.Is(err, os.ErrProcessDone) {
			t.Fatalf("owned handle remains live after Run: %v", err)
		}
	}
}

func helperCommand(mode string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessHelper$") // #nosec G204 G702 -- Relaunch only this test executable with a fixed helper selection.
	cmd.Env = append(os.Environ(), "GOPGROUTE_SUPERVISOR_HELPER="+mode, "GORACE=atexit_sleep_ms=0")
	return cmd
}

func testPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(read.Close(), write.Close()); err != nil {
			t.Error(err)
		}
	})
	return read, write
}

func readPID(t *testing.T, pipe *os.File) int {
	t.Helper()
	if err := pipe.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var pid uint64
	if err := binary.Read(pipe, binary.LittleEndian, &pid); err != nil {
		t.Fatalf("child readiness: %v", err)
	}
	if pid == 0 || pid > 1<<31-1 {
		t.Fatalf("invalid child PID: %d", pid)
	}
	return int(pid)
}

func stopHelper(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd.ProcessState != nil {
		return
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Error(err)
	}
	if err := cmd.Wait(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Error(err)
		}
	}
}

func closeFD(t *testing.T, fd int) {
	t.Helper()
	if err := unix.Close(fd); err != nil {
		t.Error(err)
	}
}
