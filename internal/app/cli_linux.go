//go:build linux

package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
)

func Run(ctx context.Context, args []string) error {
	if len(args) == 2 && args[0] == "-config" {
		cfg, err := Load(args[1])
		if err != nil {
			return err
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		speaker := exec.Command(executable, "_speaker") // #nosec G204 -- Relaunch the current executable with a fixed private role.
		adapter := exec.Command(executable, "_adapter") // #nosec G204 -- Relaunch the current executable with a fixed private role.
		for _, cmd := range []*exec.Cmd{speaker, adapter} {
			cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		}
		return Guardian(ctx, cfg, speaker, adapter)
	}
	if len(args) == 1 && (args[0] == "_speaker" || args[0] == "_adapter") {
		cfg, err := readSnapshot(os.Stdin)
		if err != nil {
			return err
		}
		if args[0] == "_speaker" {
			return Speaker(ctx, cfg)
		}
		progress := os.NewFile(3, "supervisor-progress")
		result := Adapter(ctx, cfg, progress)
		return errors.Join(result, progress.Close())
	}
	return errors.New("usage: gopgroute -config /absolute/private/config.json")
}
