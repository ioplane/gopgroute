//go:build linux

package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/ioplane/gopgroute/internal/gobgp"
	"github.com/ioplane/gopgroute/internal/health"
	"github.com/ioplane/gopgroute/internal/reconcile"
	"github.com/osrg/gobgp/v4/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestNativeSpeakerPolicyAdmitsOwnedLocalPath(t *testing.T) {
	cfg := testConfig(t)
	settings, err := snapshot(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := settings.Close(); err != nil {
			t.Error(err)
		}
	}()
	speaker := childCommand("speaker")
	speaker.Stdin = settings
	if err := speaker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := speaker.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		if err := speaker.Wait(); err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Error(err)
			}
		}
	})
	connection, err := grpc.NewClient("unix://"+cfg.socket(), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDisableRetry())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := connection.Close(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	client := api.NewGoBgpServiceClient(connection)
	if err := waitUntil(ctx, func() (bool, error) { return speakerReady(ctx, client, cfg) }); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg.socket(), 0o600); err != nil {
		t.Fatal(err)
	}
	routes, err := gobgp.Open(ctx, gobgp.Config{Write: cfg.Write, Read: cfg.Read, NextHop: cfg.NextHop, RPCTimeout: time.Second, MaxAge: time.Second}, cfg.JournalDir, cfg.socket())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := routes.Close(); err != nil {
			t.Error(err)
		}
	}()
	now := time.Now()
	// This policy oracle supplies synthetic health; native SQL is tested separately.
	if err := routes.Reconcile(ctx, health.Snapshot{Primary: health.Check{State: health.Healthy, StartedAt: now}, Pooler: health.Check{State: health.Healthy, StartedAt: now}}); err != nil {
		t.Fatal(err)
	}
	stream, err := client.ListPath(ctx, &api.ListPathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Family: &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := stream.Recv()
	if err != nil || entry.GetDestination().GetPrefix() != cfg.Write.String() {
		t.Fatalf("successful owned AddPath absent from native Loc-RIB: %v %v", entry, err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected native path: %v", err)
	}
	t.Run("frozen_speaker_cannot_renew_unchanged_intent", func(t *testing.T) {
		if err := speaker.Process.Signal(syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		var stopped syscall.WaitStatus
		pid, err := syscall.Wait4(speaker.Process.Pid, &stopped, syscall.WUNTRACED, nil)
		if err != nil || pid != speaker.Process.Pid || !stopped.Stopped() || stopped.StopSignal() != syscall.SIGSTOP {
			t.Fatalf("speaker did not enter SIGSTOP: pid=%d status=%v error=%v", pid, stopped, err)
		}
		call, stop := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer stop()
		var progress bytes.Buffer
		err = reconcile.Run(call, reconcile.Config{Interval: 10 * time.Millisecond, CycleTimeout: 100 * time.Millisecond}, func(ctx context.Context) error {
			now := time.Now()
			return routes.Reconcile(ctx, health.Snapshot{Primary: health.Check{State: health.Healthy, StartedAt: now}, Pooler: health.Check{State: health.Healthy, StartedAt: now}})
		}, &progress)
		if progress.Len() != 0 {
			t.Fatalf("frozen speaker renewed unchanged owned intent: %q", progress.String())
		}
		if err == nil || call.Err() != nil {
			t.Fatalf("speaker failure did not stop the first bounded cycle: %v (outer context: %v)", err, call.Err())
		}
	})
}
