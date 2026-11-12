package reconcile

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"testing/synctest"
	"time"
)

func TestLoopProgressFollowsCompletedCycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started, complete := make(chan struct{}, 1), make(chan struct{})
		var progress bytes.Buffer
		done := make(chan error, 1)
		go func() {
			done <- Run(ctx, Config{Interval: time.Second, CycleTimeout: 5 * time.Second}, func(context.Context) error {
				started <- struct{}{}
				<-complete
				return nil
			}, &progress)
		}()
		<-started
		synctest.Wait()
		if progress.Len() != 0 {
			t.Fatal("progress before reconciliation completed")
		}
		close(complete)
		synctest.Wait()
		if progress.String() != "+" {
			t.Fatalf("completed cycle progress = %q", progress.String())
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel = %v", err)
		}
	})
}

func TestLoopStopsOnCycleFailure(t *testing.T) {
	want := errors.New("uncertain mutation")
	var progress bytes.Buffer
	calls := 0
	err := Run(context.Background(), Config{Interval: time.Second, CycleTimeout: time.Second}, func(context.Context) error {
		calls++
		return want
	}, &progress)
	if !errors.Is(err, want) || calls != 1 || progress.Len() != 0 {
		t.Fatalf("failure: err=%v calls=%d progress=%q", err, calls, progress.String())
	}
}

func TestLoopRejectsLateSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var progress bytes.Buffer
		err := Run(context.Background(), Config{Interval: time.Second, CycleTimeout: time.Second}, func(ctx context.Context) error {
			<-ctx.Done()
			return nil
		}, &progress)
		if !errors.Is(err, context.DeadlineExceeded) || progress.Len() != 0 {
			t.Fatalf("late success: err=%v progress=%q", err, progress.String())
		}
	})
}

func TestLoopIntervalsDoNotQueueCatchUpCycles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		starts := make(chan time.Time, 3)
		completed := make(chan struct{}, 3)
		done := make(chan error, 1)
		go func() {
			done <- Run(ctx, Config{Interval: time.Second, CycleTimeout: 10 * time.Second}, func(ctx context.Context) error {
				starts <- time.Now()
				select {
				case <-completed:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}, io.Discard)
		}()
		first := <-starts
		time.Sleep(4 * time.Second)
		completed <- struct{}{}
		synctest.Wait()
		select {
		case <-starts:
			t.Fatal("queued an immediate catch-up cycle")
		default:
		}
		time.Sleep(time.Second)
		second := <-starts
		if second.Sub(first) != 5*time.Second {
			t.Fatalf("second cycle at %v after first", second.Sub(first))
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel = %v", err)
		}
	})
}

type brokenWriter struct{ short bool }

func (w brokenWriter) Write([]byte) (int, error) {
	if w.short {
		return 0, nil
	}
	return 0, io.ErrClosedPipe
}

func TestLoopStopsOnProgressFailure(t *testing.T) {
	for _, short := range []bool{false, true} {
		calls := 0
		err := Run(context.Background(), Config{Interval: time.Second, CycleTimeout: time.Second}, func(context.Context) error {
			calls++
			return nil
		}, brokenWriter{short: short})
		want := io.ErrClosedPipe
		if short {
			want = io.ErrShortWrite
		}
		if !errors.Is(err, want) || calls != 1 {
			t.Fatalf("short=%v: err=%v calls=%d", short, err, calls)
		}
	}
}

func TestLoopValidationAndPreCanceledContext(t *testing.T) {
	good := Config{Interval: time.Second, CycleTimeout: time.Second}
	cycle := func(context.Context) error { t.Fatal("unexpected cycle"); return nil }
	for _, cfg := range []Config{{}, {Interval: time.Second}, {CycleTimeout: time.Second}, {Interval: -1, CycleTimeout: time.Second}} {
		if err := Run(context.Background(), cfg, cycle, io.Discard); err == nil {
			t.Fatalf("accepted config %+v", cfg)
		}
	}
	if err := Run(context.Background(), good, nil, io.Discard); err == nil {
		t.Fatal("accepted nil cycle")
	}
	if err := Run(context.Background(), good, cycle, nil); err == nil {
		t.Fatal("accepted nil progress writer")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, good, cycle, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled = %v", err)
	}
}
