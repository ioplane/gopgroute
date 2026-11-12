//go:build linux

package app

import "testing"

func TestCLIRejectsMissingArgumentsAndMutableChildInput(t *testing.T) {
	for _, args := range [][]string{nil, {"-config"}, {"_adapter"}, {"_speaker"}, {"--unknown"}, {"-config", "/missing/gopgroute.json"}} {
		if err := Run(t.Context(), args); err == nil {
			t.Fatalf("invalid invocation accepted: %q", args)
		}
	}
}
