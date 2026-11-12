//go:build linux

package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const example = `{"speaker_binary":"/usr/local/bin/gobgpd","speaker_sha256":"0000000000000000000000000000000000000000000000000000000000000000","runtime_dir":"/run/gopgroute","journal_dir":"/var/lib/gopgroute","write":"192.0.2.100/32","read":"192.0.2.101/32","next_hop":"192.0.2.1","asn":64512,"router_id":"192.0.2.1","peers":[{"address":"192.0.2.2","asn":64513,"port":179}],"patroni_url":"http://127.0.0.1:8008","max_lag_bytes":1048576,"pooler_address":"127.0.0.1:6432","backend":"127.0.0.1:5432","database":"application","user":"route_health","password_file":"/run/credentials/gopgroute/password","probe_timeout":"100ms","rpc_timeout":"100ms","max_age":"2s","interval":"100ms","cycle_timeout":"1s","heartbeat_timeout":"3s","stop_timeout":"200ms","startup_timeout":"500ms"}`

func TestDecodeConfigAndRejectAmbiguousInputs(t *testing.T) {
	if cfg, err := Decode(strings.NewReader(example)); err != nil || cfg.ASN != 64512 {
		t.Fatalf("valid configuration rejected: %v", err)
	}
	for _, input := range []string{
		`null`, `[]`, `{}`, example + `{}`,
		strings.Replace(example, `"asn":64512`, `"asn":64512,"asn":64513`, 1),
		strings.Replace(example, `"asn":64512`, `"ASN":64512`, 1),
		strings.Replace(example, `"asn":64513`, `"asn":64513,"asn":64514`, 1),
		strings.Replace(example, `"runtime_dir":`, `"unknown":`, 1),
		strings.Replace(example, `"100ms"`, `null`, 1),
		strings.Replace(example, `192.0.2.101/32`, `192.0.2.100/32`, 1),
		strings.Replace(example, `192.0.2.100/32`, `192.0.2.0/24`, 1),
		strings.Replace(example, `127.0.0.1:6432`, `127.0.0.1:5432`, 1),
		strings.Replace(example, `"1s"`, `"3s"`, 1),
		strings.Replace(example, `"1s"`, `"-1s"`, 1),
		strings.Replace(example, `"3s"`, `"9223372036854775807s"`, 1),
		strings.Replace(example, `"192.0.2.2"`, `"::ffff:192.0.2.2"`, 1),
		strings.Replace(example, `"route_health"`, `"\ud800"`, 1),
		example + strings.Repeat(" ", 65536),
		strings.Replace(example, `"max_lag_bytes":1048576,`, "", 1),
	} {
		if _, err := Decode(strings.NewReader(input)); err == nil {
			t.Fatalf("invalid configuration accepted: %q", input[:min(len(input), 120)])
		}
	}
}

func TestPrivateFilesRejectLinksAndPermissions(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "secret")
	if err := os.WriteFile(file, []byte("test-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := privateFile(file, 64); err != nil || string(data) != "test-only" {
		t.Fatalf("private file: %v", err)
	}
	link := filepath.Join(root, "symlink")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if _, err := privateFile(link, 64); err == nil {
		t.Fatal("symlink accepted")
	}
	if _, err := privateFile(file, 1); err == nil {
		t.Fatal("oversized credential accepted")
	}
	if err := os.Chmod(file, 0o644); err != nil { // #nosec G302 -- Negative fixture must reject a world-readable credential.
		t.Fatal(err)
	}
	if _, err := privateFile(file, 64); err == nil {
		t.Fatal("world-readable credential accepted")
	}
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(file, filepath.Join(root, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := privateFile(file, 64); err == nil {
		t.Fatal("multiply linked credential accepted")
	}
}

func TestSnapshotIsImmutableAndRoundTrips(t *testing.T) {
	cfg, err := Decode(strings.NewReader(example))
	if err != nil {
		t.Fatal(err)
	}
	file, err := snapshot(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := file.WriteAt([]byte{'x'}, 0); !errors.Is(err, unix.EPERM) {
		t.Fatalf("snapshot remained writable: %v", err)
	}
	actual, err := readSnapshot(file)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(actual)
	if err != nil || string(want) != string(got) {
		t.Fatalf("snapshot changed settings: %v", err)
	}
	unsealed, err := os.CreateTemp(t.TempDir(), "config")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := unsealed.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := readSnapshot(unsealed); err == nil {
		t.Fatal("child accepted mutable settings")
	}
}
