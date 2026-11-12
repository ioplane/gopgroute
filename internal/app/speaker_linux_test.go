//go:build linux

package app

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/osrg/gobgp/v4/pkg/config"
	"github.com/osrg/gobgp/v4/pkg/server"
)

func TestSpeakerRejectsNonELFWithMatchingDigest(t *testing.T) {
	cfg := testConfig(t)
	data := []byte("not an ELF executable")
	cfg.SpeakerBinary = filepath.Join(t.TempDir(), "invalid-executable")
	if err := os.WriteFile(cfg.SpeakerBinary, data, 0o700); err != nil { // #nosec G306 -- Negative fixture requires executable mode to test the ELF admission boundary.
		t.Fatal(err)
	}
	cfg.SpeakerSHA256 = fmt.Sprintf("%x", sha256.Sum256(data))
	if err := cfg.verifySpeaker(); err == nil {
		t.Fatal("non-ELF executable admitted")
	}
}

// The exec target uses the pinned native config loader and server, without a
// fake RIB. Exact upstream gobgpd artifact admission remains a separate gate.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && (os.Args[1] == "_speaker" || os.Args[1] == "_adapter") {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer cancel()
		if err := Run(ctx, os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "--api-hosts" {
		flags := flag.NewFlagSet("native-speaker", flag.ContinueOnError)
		address := flags.String("api-hosts", "", "")
		path := flags.String("config-file", "", "")
		kind := flags.String("config-type", "", "")
		flags.Bool("pprof-disable", false, "")
		flags.String("metrics-path", "", "")
		if err := flags.Parse(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		cfg, err := config.ReadConfigFile(*path, *kind)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		s := server.NewBgpServer(server.GrpcListenAddress(*address))
		go s.Serve()
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer cancel()
		if _, err := config.InitialConfig(ctx, s, cfg, false); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		<-ctx.Done()
		os.Exit(0)
	}
	os.Exit(m.Run())
}
