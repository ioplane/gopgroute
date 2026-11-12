//go:build linux

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/osrg/gobgp/v4/api"
	"golang.org/x/sys/unix"
)

func (cfg Config) verifySpeaker() error {
	file, err := cfg.openSpeaker()
	if err != nil {
		return err
	}
	return file.Close()
}

func (cfg Config) openSpeaker() (file *os.File, result error) {
	fd, err := unix.Open(cfg.SpeakerBinary, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file = os.NewFile(uintptr(fd), "speaker-executable")
	defer func() {
		if result != nil {
			result = errors.Join(result, file.Close())
			file = nil
		}
	}()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return file, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != 0 || stat.Mode&0o6022 != 0 || stat.Mode&0o111 == 0 || stat.Nlink != 1 {
		return file, errors.New("speaker executable must be root-owned, executable, non-writable by group/other and without set-id")
	}
	if size, err := unix.Fgetxattr(fd, "security.capability", nil); !errors.Is(err, unix.ENODATA) || size > 0 {
		return file, errors.New("speaker file capabilities must be absent")
	}
	var magic [4]byte
	if _, err := io.ReadFull(file, magic[:]); err != nil || !bytes.Equal(magic[:], []byte{0x7f, 'E', 'L', 'F'}) {
		return file, errors.New("speaker must be an ELF executable")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return file, err
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return file, err
	}
	if hex.EncodeToString(digest.Sum(nil)) != cfg.SpeakerSHA256 {
		return file, errors.New("speaker digest mismatch")
	}
	return file, nil
}

// Interpolated values are validated numeric fields or netip values.
func speakerConfig(cfg Config) []byte {
	var text strings.Builder
	fmt.Fprintf(&text, `[global.config]
as = %d
router-id = "%s"
port = -1
[global.apply-policy.config]
default-import-policy = "reject-route"
default-export-policy = "reject-route"
import-policy-list = ["local-services"]
export-policy-list = ["local-services"]
[[defined-sets.prefix-sets]]
prefix-set-name = "services"
[[defined-sets.prefix-sets.prefix-list]]
ip-prefix = "%s"
masklength-range = "32..32"
[[defined-sets.prefix-sets.prefix-list]]
ip-prefix = "%s"
masklength-range = "32..32"
[[policy-definitions]]
name = "local-services"
[[policy-definitions.statements]]
name = "local-services"
[policy-definitions.statements.conditions.bgp-conditions]
route-type = "local"
[policy-definitions.statements.conditions.match-prefix-set]
prefix-set = "services"
match-set-options = "any"
[policy-definitions.statements.actions]
route-disposition = "accept-route"
`, cfg.ASN, cfg.RouterID, cfg.Write, cfg.Read)
	for _, peer := range cfg.Peers {
		fmt.Fprintf(&text, `[[neighbors]]
[neighbors.config]
neighbor-address = "%s"
peer-as = %d
[neighbors.transport.config]
remote-port = %d
[neighbors.graceful-restart.config]
enabled = false
[[neighbors.afi-safis]]
[neighbors.afi-safis.config]
afi-safi-name = "ipv4-unicast"
enabled = true
`, peer.Address, peer.ASN, peer.Port)
	}
	return []byte(text.String())
}

// Read-only retries cover upstream's asynchronous initial configuration.
// No route manager is opened until every static admission check succeeds.
func speakerReady(ctx context.Context, client api.GoBgpServiceClient, cfg Config) (bool, error) {
	state, err := client.GetBgp(ctx, &api.GetBgpRequest{})
	if err != nil {
		return false, nil
	} // fail-silent-ok: bounded read-only readiness retry; never authorizes a route.
	if state.GetGlobal().GetAsn() != cfg.ASN || state.GetGlobal().GetRouterId() != cfg.RouterID.String() {
		return false, nil
	}
	for _, direction := range []api.PolicyDirection{api.PolicyDirection_POLICY_DIRECTION_IMPORT, api.PolicyDirection_POLICY_DIRECTION_EXPORT} {
		stream, err := client.ListPolicyAssignment(ctx, &api.ListPolicyAssignmentRequest{Name: "global", Direction: direction})
		if err != nil {
			return false, err
		}
		entry, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		assignment := entry.GetAssignment()
		if assignment.GetDefaultAction() != api.RouteAction_ROUTE_ACTION_REJECT {
			return false, nil
		}
		policies := assignment.GetPolicies()
		if len(policies) != 1 || policies[0].GetName() != "local-services" {
			return false, nil
		}
		if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
			return false, errors.New("unexpected policy assignment stream")
		}
	}
	policies, err := client.ListPolicy(ctx, &api.ListPolicyRequest{Name: "local-services"})
	if err != nil {
		return false, err
	}
	p, err := policies.Recv()
	if errors.Is(err, io.EOF) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	statements := p.GetPolicy().GetStatements()
	if len(statements) != 1 {
		return false, nil
	}
	condition := statements[0].GetConditions()
	if condition.GetPrefixSet().GetName() != "services" || condition.GetPrefixSet().GetType() != api.MatchSet_TYPE_ANY || condition.GetRouteType() != api.Conditions_ROUTE_TYPE_LOCAL || statements[0].GetActions().GetRouteAction() != api.RouteAction_ROUTE_ACTION_ACCEPT {
		return false, nil
	}
	if _, err := policies.Recv(); !errors.Is(err, io.EOF) {
		return false, errors.New("unexpected export policy stream")
	}
	sets, err := client.ListDefinedSet(ctx, &api.ListDefinedSetRequest{DefinedType: api.DefinedType_DEFINED_TYPE_PREFIX, Name: "services"})
	if err != nil {
		return false, err
	}
	set, err := sets.Recv()
	if errors.Is(err, io.EOF) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	prefixes := set.GetDefinedSet().GetPrefixes()
	if len(prefixes) != 2 {
		return false, nil
	}
	seen := make(map[string]bool)
	for _, prefix := range prefixes {
		if (prefix.GetIpPrefix() != cfg.Write.String() && prefix.GetIpPrefix() != cfg.Read.String()) || prefix.GetMaskLengthMin() != 32 || prefix.GetMaskLengthMax() != 32 || seen[prefix.GetIpPrefix()] {
			return false, nil
		}
		seen[prefix.GetIpPrefix()] = true
	}
	if _, err := sets.Recv(); !errors.Is(err, io.EOF) {
		return false, errors.New("unexpected prefix set stream")
	}
	peers, err := client.ListPeer(ctx, &api.ListPeerRequest{})
	if err != nil {
		return false, err
	}
	peerSeen := make(map[string]bool)
	for {
		entry, err := peers.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false, err
		}
		peer := entry.GetPeer()
		address := peer.GetConf().GetNeighborAddress()
		matched := false
		for _, expected := range cfg.Peers {
			if address == expected.Address.String() && peer.GetConf().GetPeerAsn() == expected.ASN && peer.GetTransport().GetRemotePort() == uint32(expected.Port) && !peer.GetGracefulRestart().GetEnabled() {
				matched = true
			}
		}
		if !matched || peerSeen[address] {
			return false, nil
		}
		peerSeen[address] = true
	}
	return len(peerSeen) == len(cfg.Peers), nil
}
