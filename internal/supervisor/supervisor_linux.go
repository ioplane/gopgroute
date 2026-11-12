//go:build linux

// Package supervisor owns one dedicated speaker and adapter process incarnation.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"time"
)

// Config transfers exclusive ownership of two unstarted, direct child commands
// to Run. Only nil or *os.File standard streams are allowed, so exec cannot
// create copying goroutines that wait on arbitrary caller I/O. ExtraFiles,
// Cancel and SysProcAttr must be unset. The adapter receives its private
// heartbeat pipe at fd 3 and must send '+' after each completed reconciliation.
// OwnershipFiles remain caller-owned and are inherited by both children (speaker
// fd 3+i, adapter fd 4+i). They must stay open through exec and must never be unlocked.
type Config struct {
	Speaker, Adapter              *exec.Cmd
	OwnershipFiles                []*os.File
	HeartbeatTimeout, StopTimeout time.Duration
}

// Run binds one adapter lifetime to one dedicated speaker incarnation. Any
// child exit, missing or malformed heartbeat, or cancellation stops both
// children. StopTimeout bounds each TERM and KILL wait; an unreapable kernel
// task is reported as an error rather than blocking the supervisor forever.
// Run does not restart children, withdraw individual routes or qualify peer GR.
func Run(ctx context.Context, cfg Config) (err error) {
	if err := validate(cfg); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("start supervision: %w", err)
	}
	// Linux Pdeathsig follows the creating thread, not just the process.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	read, write, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create heartbeat pipe: %w", err)
	}
	var speaker, adapter *child
	var readerDone chan struct{}
	defer func() {
		err = errors.Join(err, stopOwned(speaker, adapter, cfg.StopTimeout))
		err = errors.Join(err, closePipe(read), closePipe(write))
		if readerDone != nil {
			<-readerDone
		}
	}()
	cfg.Speaker.ExtraFiles = append([]*os.File(nil), cfg.OwnershipFiles...)
	speaker, err = startOwned("speaker", cfg.Speaker, cfg.StopTimeout)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("start adapter: %w", err)
	}
	cfg.Adapter.ExtraFiles = append([]*os.File{write}, cfg.OwnershipFiles...)
	adapter, err = startOwned("adapter", cfg.Adapter, cfg.StopTimeout)
	if err != nil {
		return err
	}
	if err := write.Close(); err != nil {
		return fmt.Errorf("close inherited heartbeat writer: %w", err)
	}
	write = nil

	progress := &heartbeatProgress{last: time.Now()}
	heartbeatError := make(chan error, 1)
	readerDone = make(chan struct{})
	go func() {
		defer close(readerDone)
		heartbeatError <- readHeartbeats(read, progress, cfg.HeartbeatTimeout)
	}()
	timer := time.NewTimer(cfg.HeartbeatTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("supervision canceled: %w", ctx.Err())
		case err := <-speaker.done:
			speaker.done = nil
			return unexpectedExit(speaker.name, err)
		case err := <-adapter.done:
			adapter.done = nil
			return unexpectedExit(adapter.name, err)
		case err := <-heartbeatError:
			return err
		case <-timer.C:
			progress.mu.Lock()
			remaining := cfg.HeartbeatTimeout - time.Since(progress.last)
			progress.mu.Unlock()
			if remaining <= 0 {
				return errors.New("adapter heartbeat timeout")
			}
			timer.Reset(remaining)
		}
	}
}

func validate(cfg Config) error {
	if cfg.Speaker == nil || cfg.Adapter == nil || cfg.Speaker == cfg.Adapter {
		return errors.New("supervision requires two distinct commands")
	}
	if cfg.HeartbeatTimeout <= 0 || cfg.StopTimeout <= 0 {
		return errors.New("supervision requires positive heartbeat and stop timeouts")
	}
	for _, file := range cfg.OwnershipFiles {
		if file == nil {
			return errors.New("nil ownership file")
		}
		if _, err := file.Stat(); err != nil {
			return fmt.Errorf("ownership file: %w", err)
		}
	}
	for _, cmd := range []*exec.Cmd{cfg.Speaker, cfg.Adapter} {
		if cmd.Process != nil || len(cmd.ExtraFiles) != 0 || cmd.Cancel != nil || cmd.SysProcAttr != nil {
			return errors.New("supervision requires unstarted commands without extra files, Cancel or process attributes")
		}
		for _, stream := range []any{cmd.Stdin, cmd.Stdout, cmd.Stderr} {
			if stream != nil {
				if file, ok := stream.(*os.File); !ok || file == nil {
					return errors.New("supervised command standard streams must be nil or non-nil *os.File")
				}
			}
		}
	}
	return nil
}

type child struct {
	name string
	cmd  *exec.Cmd
	done chan error
}

func startOwned(name string, cmd *exec.Cmd, timeout time.Duration) (*child, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	cmd.WaitDelay = timeout
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	c := &child{name: name, cmd: cmd, done: make(chan error, 1)}
	go func() { c.done <- cmd.Wait() }()
	return c, nil
}

type heartbeatProgress struct {
	mu   sync.Mutex
	last time.Time
}

func readHeartbeats(pipe *os.File, progress *heartbeatProgress, timeout time.Duration) error {
	var buffer [4096]byte
	for {
		n, err := pipe.Read(buffer[:])
		if err != nil {
			return fmt.Errorf("read adapter heartbeat: %w", err)
		}
		if n == 0 {
			return errors.New("empty adapter heartbeat read")
		}
		for _, b := range buffer[:n] {
			if b != '+' {
				return fmt.Errorf("invalid adapter heartbeat byte: %x", b)
			}
		}
		progress.mu.Lock()
		now := time.Now()
		if now.Sub(progress.last) >= timeout {
			progress.mu.Unlock()
			return errors.New("adapter heartbeat timeout")
		}
		progress.last = now
		progress.mu.Unlock()
	}
}

func unexpectedExit(name string, err error) error {
	if err == nil {
		return fmt.Errorf("%s exited unexpectedly", name)
	}
	return fmt.Errorf("%s exited: %w", name, err)
}

func stopOwned(speaker, adapter *child, timeout time.Duration) error {
	children := []*child{speaker, adapter}
	err := signalOwned(children, syscall.SIGTERM)
	done, waitErr := waitOwned(speaker, adapter, timeout)
	err = errors.Join(err, waitErr)
	if done {
		return err
	}
	err = errors.Join(err, signalOwned(children, syscall.SIGKILL))
	done, waitErr = waitOwned(speaker, adapter, timeout)
	err = errors.Join(err, waitErr)
	if !done {
		err = errors.Join(err, errors.New("owned child did not exit within SIGKILL wait bound"))
	}
	return err
}

func signalOwned(children []*child, sig syscall.Signal) error {
	var result error
	for _, c := range children {
		if c == nil || c.done == nil {
			continue
		}
		if err := c.cmd.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
			result = errors.Join(result, fmt.Errorf("signal owned %s: %w", c.name, err))
		}
	}
	return result
}

func waitOwned(speaker, adapter *child, timeout time.Duration) (bool, error) {
	var speakerDone, adapterDone <-chan error
	if speaker != nil {
		speakerDone = speaker.done
	}
	if adapter != nil {
		adapterDone = adapter.done
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var result error
	for speakerDone != nil || adapterDone != nil {
		select {
		case err := <-speakerDone:
			speaker.done, speakerDone = nil, nil
			result = errors.Join(result, checkWait(speaker.name, err))
		case err := <-adapterDone:
			adapter.done, adapterDone = nil, nil
			result = errors.Join(result, checkWait(adapter.name, err))
		case <-timer.C:
			return false, result
		}
	}
	return true, result
}

func checkWait(name string, err error) error {
	var exit *exec.ExitError
	if err == nil || errors.As(err, &exit) {
		return nil // fail-silent-ok: cleanup expects the requested TERM or KILL exit.
	}
	return fmt.Errorf("wait owned %s: %w", name, err)
}

func closePipe(pipe *os.File) error {
	if pipe == nil {
		return nil
	}
	if err := pipe.Close(); err != nil {
		return fmt.Errorf("close heartbeat pipe: %w", err)
	}
	return nil
}
