//go:build linux

package app

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// A lease is never unlocked explicitly: children inherit the same open file
// description, and only the last close may admit the next incarnation.
func incarnationLease(dir string) (file *os.File, result error) {
	path := filepath.Join(dir, "guardian.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600) // #nosec G304 -- Fixed basename under admitted private root; no-follow, inode, owner and mode checked before locking.
	if err != nil {
		return nil, err
	}
	defer func() {
		if result != nil {
			result = errors.Join(result, file.Close())
			file = nil
		}
	}()
	info, err := file.Stat()
	linked, linkErr := os.Lstat(path)
	if err != nil || linkErr != nil {
		return file, errors.Join(err, linkErr)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode() != 0o600 || int64(stat.Uid) != int64(os.Geteuid()) || stat.Nlink != 1 || !os.SameFile(info, linked) {
		return file, errors.New("invalid incarnation lease identity")
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return file, fmt.Errorf("lock incarnation: %w", err)
	}
	if err := file.Sync(); err != nil {
		return file, err
	}
	return file, nil
}

func incarnationDirectory(root, name string) (result error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "guardian.lock" {
			continue
		}
		id := entry.Name()
		if len(id) != 34 || id[:2] != "i-" {
			return errors.New("legacy or unknown incarnation root entry; explicit migration required")
		}
		value, err := hex.DecodeString(id[2:])
		if err != nil || hex.EncodeToString(value) != id[2:] {
			return errors.New("invalid incarnation directory name")
		}
		if err := privateDir(filepath.Join(root, id)); err != nil {
			return err
		}
	}
	path := filepath.Join(root, name)
	if err := os.Mkdir(path, 0o700); err != nil {
		return err
	}
	// Retain a partially prepared directory after any failure; never reuse it.
	for _, dir := range []string{path, root} {
		file, err := os.Open(dir) // #nosec G304 -- Admitted service-owned root or freshly created private generation; synchronize before children start.
		if err != nil {
			return err
		}
		if err := errors.Join(file.Sync(), file.Close()); err != nil {
			return err
		}
	}
	return nil
}

func newIncarnation(cfg Config) (Config, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Config{}, err
	}
	name := "i-" + hex.EncodeToString(id[:])
	next := cfg
	next.RuntimeDir, next.JournalDir = filepath.Join(cfg.RuntimeDir, name), filepath.Join(cfg.JournalDir, name)
	if err := next.validate(); err != nil {
		return Config{}, err
	}
	for _, root := range []string{cfg.RuntimeDir, cfg.JournalDir} {
		if err := incarnationDirectory(root, name); err != nil {
			return Config{}, err
		}
	}
	return next, nil
}
