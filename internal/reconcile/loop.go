// Package reconcile reports supervisor progress only after a completed route cycle.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

type Config struct {
	Interval, CycleTimeout time.Duration
}

// Run serializes complete health/route cycles. The caller supplies the inherited
// supervisor pipe; a separate process must bound callbacks or writes that stall.
func Run(ctx context.Context, cfg Config, cycle func(context.Context) error, progress io.Writer) error {
	if cfg.Interval <= 0 || cfg.CycleTimeout <= 0 || cycle == nil || progress == nil {
		return errors.New("invalid reconciliation configuration")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		call, cancel := context.WithTimeout(ctx, cfg.CycleTimeout)
		err := cycle(call)
		err = errors.Join(err, call.Err())
		cancel()
		if err != nil {
			return fmt.Errorf("reconcile routes: %w", err)
		}
		if count, err := progress.Write([]byte{'+'}); err != nil {
			return fmt.Errorf("report reconciliation progress: %w", err)
		} else if count != 1 {
			return fmt.Errorf("report reconciliation progress: %w", io.ErrShortWrite)
		}
		timer := time.NewTimer(cfg.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
