package gobgp

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

type (
	entry struct {
		Phase string
		UUID  string
	}
	journal struct {
		Version              int
		Write, Read, NextHop string
		Entries              [2]entry
	}
	store struct {
		root            *os.Root
		directory, lock *os.File
	}
)

func private(info os.FileInfo, mode os.FileMode) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int64(stat.Uid) == int64(os.Geteuid()) && info.Mode().Perm() == mode && info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0
}

func openStore(dir string) (*store, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || !private(info, 0o700) {
		return nil, errors.New("state directory must be private and service-owned")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	s := &store{root: root}
	s.directory, err = root.Open(".")
	if err != nil {
		return nil, errors.Join(err, s.close())
	}
	opened, err := s.directory.Stat()
	if err != nil || !opened.IsDir() || !private(opened, 0o700) || !os.SameFile(info, opened) {
		return nil, errors.Join(errors.New("state directory changed during open"), err, s.close())
	}
	s.lock, err = root.OpenFile("lock", os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, errors.Join(err, s.close())
	}
	info, err = s.lock.Stat()
	linkInfo, linkErr := root.Lstat("lock")
	if err != nil || linkErr != nil || !regularPrivate(info) || !regularPrivate(linkInfo) || !os.SameFile(info, linkInfo) {
		return nil, errors.Join(errors.New("invalid state lock"), err, s.close())
	}
	if err = unix.Flock(int(s.lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.Join(err, s.close())
	}
	if _, err = root.Lstat("journal.next"); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.Join(ErrRecovery, errors.New("staging journal exists or cannot be inspected"), err, s.close())
	}
	return s, nil
}

func regularPrivate(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || !private(info, 0o600) {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}

func (s *store) load() (journal, error) {
	f, err := s.root.OpenFile("journal.json", os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return journal{}, err
	}
	info, statErr := f.Stat()
	linkInfo, linkErr := s.root.Lstat("journal.json")
	if statErr != nil || linkErr != nil || !regularPrivate(info) || !regularPrivate(linkInfo) || !os.SameFile(info, linkInfo) {
		return journal{}, errors.Join(errors.New("invalid journal file"), statErr, f.Close())
	}
	data, readErr := io.ReadAll(io.LimitReader(f, 64*1024+1))
	closeErr := f.Close()
	if err = errors.Join(readErr, closeErr); err != nil {
		return journal{}, err
	}
	var j journal
	if err = json.Unmarshal(data, &j); err != nil {
		return j, fmt.Errorf("decode journal: %w", err)
	}
	canonical, err := json.Marshal(j)
	if err != nil {
		return j, err
	}
	if !bytes.Equal(data, append(canonical, '\n')) {
		return j, errors.New("journal is not canonical")
	}
	for _, e := range j.Entries {
		switch e.Phase {
		case "", "pending-add":
			if e.UUID != "" {
				return j, errors.New("unexpected journal UUID")
			}
		case "owned", "pending-delete":
			if _, err := decodeUUID(e.UUID); err != nil {
				return j, err
			}
		default:
			return j, errors.New("invalid journal phase")
		}
	}
	return j, nil
}

func (s *store) save(j journal) error {
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	f, err := s.root.OpenFile("journal.next", os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(append(data, '\n'))
	syncErr := f.Sync()
	closeErr := f.Close()
	if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err = s.root.Rename("journal.next", "journal.json"); err != nil {
		return err
	}
	return s.directory.Sync()
}

func (s *store) close() error {
	var errs []error
	if s.lock != nil {
		errs = append(errs, s.lock.Close())
	}
	if s.directory != nil {
		errs = append(errs, s.directory.Close())
	}
	if s.root != nil {
		errs = append(errs, s.root.Close())
	}
	return errors.Join(errs...)
}

func decodeUUID(value string) ([]byte, error) {
	uuid, err := hex.DecodeString(value)
	if err != nil || !validUUID(uuid) || hex.EncodeToString(uuid) != value {
		return nil, errors.New("invalid path UUID")
	}
	return uuid, nil
}
func validUUID(uuid []byte) bool { return len(uuid) == 16 && !bytes.Equal(uuid, make([]byte, 16)) }
