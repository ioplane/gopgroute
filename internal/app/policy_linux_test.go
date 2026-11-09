//go:build linux

package app

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/ioplane/gopgroute/internal/gobgp"
	"github.com/ioplane/gopgroute/internal/health"
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
}
