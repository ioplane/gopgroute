//go:build linux

// Package app wires one supervised route-adapter incarnation.
package app

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ioplane/gopgroute/internal/health"
	"golang.org/x/sys/unix"
)

const configLimit = 64 << 10

type Duration time.Duration

func (d *Duration) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return errors.New("duration must be a string")
	}
	value, err := time.ParseDuration(text)
	if err != nil || value <= 0 {
		return errors.New("duration must be positive and finite")
	}
	*d = Duration(value)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

type Peer struct {
	Address netip.Addr `json:"address"`
	ASN     uint32     `json:"asn"`
	Port    uint16     `json:"port"`
}

type Config struct {
	guardianPID      int
	SpeakerBinary    string         `json:"speaker_binary"`
	SpeakerSHA256    string         `json:"speaker_sha256"`
	RuntimeDir       string         `json:"runtime_dir"`
	JournalDir       string         `json:"journal_dir"`
	Write            netip.Prefix   `json:"write"`
	Read             netip.Prefix   `json:"read"`
	NextHop          netip.Addr     `json:"next_hop"`
	ASN              uint32         `json:"asn"`
	RouterID         netip.Addr     `json:"router_id"`
	Peers            []Peer         `json:"peers"`
	PatroniURL       string         `json:"patroni_url"`
	MaxLagBytes      uint64         `json:"max_lag_bytes"`
	PoolerAddress    netip.AddrPort `json:"pooler_address"`
	Backend          netip.AddrPort `json:"backend"`
	Database         string         `json:"database"`
	User             string         `json:"user"`
	PasswordFile     string         `json:"password_file"`
	ProbeTimeout     Duration       `json:"probe_timeout"`
	RPCTimeout       Duration       `json:"rpc_timeout"`
	MaxAge           Duration       `json:"max_age"`
	Interval         Duration       `json:"interval"`
	CycleTimeout     Duration       `json:"cycle_timeout"`
	HeartbeatTimeout Duration       `json:"heartbeat_timeout"`
	StopTimeout      Duration       `json:"stop_timeout"`
	StartupTimeout   Duration       `json:"startup_timeout"`
}

// Decode accepts a bounded, unambiguous schema; diagnostics never echo input.
func Decode(reader io.Reader) (Config, error) {
	data, err := io.ReadAll(io.LimitReader(reader, configLimit+1))
	if err != nil || len(data) > configLimit || !utf8.Valid(data) {
		return Config{}, errors.New("invalid configuration bytes or size")
	}
	guard := json.NewDecoder(bytes.NewReader(data))
	guard.UseNumber()
	if err := jsonValue(guard, 0); err != nil {
		return Config{}, errors.New("ambiguous configuration JSON")
	}
	if _, err := guard.Token(); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("trailing configuration JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return Config{}, errors.New("configuration must be an object")
	}
	if _, present := fields["max_lag_bytes"]; !present {
		return Config{}, errors.New("missing max_lag_bytes")
	}
	decoder.DisallowUnknownFields()
	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, errors.New("invalid configuration schema")
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func jsonValue(decoder *json.Decoder, depth int) error {
	if depth > 4 {
		return errors.New("JSON nesting limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch token {
	case nil:
		return errors.New("null configuration value")
	case json.Delim('{'):
		seen := make(map[string]bool)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] || name != strings.ToLower(name) {
				return errors.New("ambiguous configuration name")
			}
			seen[name] = true
			if err := jsonValue(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err := decoder.Token()
		return err
	case json.Delim('['):
		for decoder.More() {
			if err := jsonValue(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err := decoder.Token()
		return err
	default:
		if value, ok := token.(string); ok && strings.ContainsRune(value, utf8.RuneError) {
			return errors.New("invalid configuration Unicode")
		}
		return nil
	}
}

func (cfg Config) validate() error {
	digest, err := hex.DecodeString(cfg.SpeakerSHA256)
	if err != nil || len(digest) != 32 || hex.EncodeToString(digest) != cfg.SpeakerSHA256 {
		return errors.New("invalid speaker digest")
	}
	for _, path := range []string{cfg.RuntimeDir, cfg.JournalDir, cfg.PasswordFile, cfg.SpeakerBinary} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
			return errors.New("configuration paths must be absolute and canonical")
		}
	}
	if cfg.RuntimeDir == cfg.JournalDir || len(filepath.Join(cfg.RuntimeDir, "speaker.sock")) > 107 {
		return errors.New("invalid runtime or journal directory")
	}
	validIP := func(ip netip.Addr) bool { return ip.Is4() && ip.IsGlobalUnicast() }
	if !validIP(cfg.Write.Addr()) || !validIP(cfg.Read.Addr()) || cfg.Write.Bits() != 32 || cfg.Read.Bits() != 32 || cfg.Write == cfg.Read || !validIP(cfg.NextHop) || !validIP(cfg.RouterID) || cfg.ASN == 0 {
		return errors.New("invalid service prefixes, next hop or speaker identity")
	}
	if len(cfg.Peers) < 1 || len(cfg.Peers) > 16 {
		return errors.New("require one to sixteen peers")
	}
	seen := make(map[netip.Addr]bool)
	for _, peer := range cfg.Peers {
		if !peer.Address.Is4() || peer.Address.IsUnspecified() || peer.Address.IsMulticast() || peer.Address.Zone() != "" || peer.ASN == 0 || peer.ASN == cfg.ASN || peer.Port == 0 || seen[peer.Address] {
			return errors.New("invalid or duplicate external IPv4 peer")
		}
		seen[peer.Address] = true
	}
	for name, duration := range map[string]Duration{"probe_timeout": cfg.ProbeTimeout, "rpc_timeout": cfg.RPCTimeout, "max_age": cfg.MaxAge, "interval": cfg.Interval, "cycle_timeout": cfg.CycleTimeout, "heartbeat_timeout": cfg.HeartbeatTimeout, "stop_timeout": cfg.StopTimeout, "startup_timeout": cfg.StartupTimeout} {
		if duration <= 0 {
			return fmt.Errorf("missing positive %s", name)
		}
	}
	if cfg.CycleTimeout >= cfg.HeartbeatTimeout || cfg.StartupTimeout >= cfg.HeartbeatTimeout-cfg.CycleTimeout || cfg.Interval >= cfg.HeartbeatTimeout-cfg.CycleTimeout || cfg.ProbeTimeout > cfg.CycleTimeout/3 || cfg.RPCTimeout > cfg.CycleTimeout || cfg.MaxAge < cfg.CycleTimeout {
		return errors.New("inconsistent reconciliation time budgets")
	}
	local := func(a netip.AddrPort) bool {
		return a.IsValid() && a.Port() != 0 && a.Addr().IsLoopback() && !a.Addr().Is4In6() && a.Addr().Zone() == ""
	}
	if !local(cfg.PoolerAddress) || !local(cfg.Backend) || cfg.PoolerAddress == cfg.Backend || cfg.Database == "" || cfg.Database == "pgbouncer" || len(cfg.Database) > 63 || cfg.User == "" || len(cfg.User) > 63 || strings.ContainsRune(cfg.Database+cfg.User, 0) {
		return errors.New("invalid pooler identity or endpoints")
	}
	_, err = health.NewPatroni(cfg.PatroniURL, time.Duration(cfg.ProbeTimeout), nil)
	return err
}

func privateDir(path string) (result error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open private directory: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() { result = errors.Join(result, file.Close()) }()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if int64(stat.Uid) != int64(os.Geteuid()) || stat.Mode&0o7777 != 0o700 {
		return errors.New("directory must be service-owned mode 0700")
	}
	return nil
}

func privateFile(path string, limit int64) (data []byte, result error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open private file: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() { result = errors.Join(result, file.Close()) }()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o7777 != 0o600 || int64(stat.Uid) != int64(os.Geteuid()) || stat.Nlink != 1 {
		return nil, errors.New("file must be private, regular, service-owned and single-link")
	}
	data, err = io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || len(data) == 0 || int64(len(data)) > limit {
		return nil, errors.New("invalid private file read or size")
	}
	return data, nil
}

const seals = unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL

type childSettings struct {
	Config      Config `json:"config"`
	GuardianPID int    `json:"guardian_pid"`
}

func snapshot(cfg Config) (file *os.File, result error) {
	data, err := json.Marshal(childSettings{Config: cfg, GuardianPID: os.Getpid()})
	if err != nil {
		return nil, err
	}
	if len(data) > configLimit+1024 {
		return nil, errors.New("child settings exceed size limit")
	}
	return sealed(data)
}

func sealed(data []byte) (file *os.File, result error) {
	fd, err := unix.MemfdCreate("gopgroute-settings", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, err
	}
	file = os.NewFile(uintptr(fd), "gopgroute-settings")
	defer func() {
		if result != nil {
			result = errors.Join(result, file.Close())
			file = nil
		}
	}()
	if _, err := file.Write(data); err != nil {
		return file, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return file, err
	}
	if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, seals); err != nil {
		return file, err
	}
	return file, nil
}

func readSnapshot(file *os.File) (Config, error) {
	actual, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
	if err != nil || actual&seals != seals {
		return Config{}, errors.New("child requires sealed settings")
	}
	data, err := io.ReadAll(io.LimitReader(file, configLimit+1025))
	if err != nil || len(data) > configLimit+1024 {
		return Config{}, errors.New("invalid child settings size or read")
	}
	var settings childSettings
	if err := json.Unmarshal(data, &settings); err != nil || settings.GuardianPID <= 0 || int64(settings.GuardianPID) > 1<<31-1 {
		return Config{}, errors.New("invalid child settings")
	}
	canonical, err := json.Marshal(settings)
	if err != nil || !bytes.Equal(canonical, data) {
		return Config{}, errors.New("noncanonical child settings")
	}
	if err := settings.Config.validate(); err != nil {
		return Config{}, err
	}
	settings.Config.guardianPID = settings.GuardianPID
	return settings.Config, nil
}
