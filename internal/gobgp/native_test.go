package gobgp

import (
	"bytes"
	"context"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func nativeClient(t *testing.T) api.GoBgpServiceClient {
	t.Helper()
	client, _, _ := nativeEndpoint(t)
	return client
}

func nativeEndpoint(t *testing.T) (api.GoBgpServiceClient, string, func()) {
	t.Helper()
	socket := filepath.Join(privateDir(t), "rpc.sock")
	speaker := server.NewBgpServer(server.GrpcListenAddress("unix://" + socket))
	go speaker.Serve()
	var once sync.Once
	stop := func() { once.Do(speaker.Stop) }
	t.Cleanup(stop)
	connection, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDisableRetry())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := connection.Close(); err != nil {
			t.Error(err)
		}
	})
	client := api.NewGoBgpServiceClient(connection)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err = client.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: 64512, RouterId: "192.0.2.254", ListenPort: -1}}, grpc.WaitForReady(true))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	return client, socket, stop
}

func ipv4() *api.Family { return &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST} }

func nativeAdd(t *testing.T, c api.GoBgpServiceClient, prefix string) []byte {
	t.Helper()
	nlri, err := bgp.NewIPAddrPrefix(netip.MustParsePrefix(prefix))
	if err != nil {
		t.Fatal(err)
	}
	nextHop, err := bgp.NewPathAttributeNextHop(netip.MustParseAddr("192.0.2.1"))
	if err != nil {
		t.Fatal(err)
	}
	path, err := apiutil.NewPath(bgp.RF_IPv4_UC, nlri, false, []bgp.PathAttributeInterface{bgp.NewPathAttributeOrigin(0), nextHop}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	response, err := c.AddPath(ctx, &api.AddPathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Uuid) != 16 || bytes.Equal(response.Uuid, make([]byte, 16)) {
		t.Fatalf("invalid native UUID: %x", response.Uuid)
	}
	return response.Uuid
}

func nativeList(t *testing.T, c api.GoBgpServiceClient) []*api.Destination {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	stream, err := c.ListPath(ctx, &api.ListPathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Family: ipv4()})
	if err != nil {
		t.Fatal(err)
	}
	var result []*api.Destination
	for {
		response, err := stream.Recv()
		if err == io.EOF {
			return result
		}
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, response.Destination)
	}
}

func TestNativeListPathDoesNotRecoverUUID(t *testing.T) {
	c := nativeClient(t)
	nativeAdd(t, c, "192.0.2.100/32")
	paths := nativeList(t, c)
	if len(paths) != 1 || len(paths[0].Paths) != 1 {
		t.Fatalf("unexpected RIB: %+v", paths)
	}
	if len(paths[0].Paths[0].Uuid) != 0 {
		t.Fatal("GoBGP ListPath UUID contract changed; reassess recovery design")
	}
}

func TestNativeUUIDDeletePreservesForeignPath(t *testing.T) {
	c := nativeClient(t)
	own := nativeAdd(t, c, "192.0.2.100/32")
	nativeAdd(t, c, "192.0.2.101/32")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := c.DeletePath(ctx, &api.DeletePathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Uuid: own}); err != nil {
		t.Fatal(err)
	}
	paths := nativeList(t, c)
	if len(paths) != 1 || paths[0].Prefix != "192.0.2.101/32" {
		t.Fatalf("foreign route changed: %+v", paths)
	}
}

func TestNativeEmptyUUIDWouldDeleteForeignFamily(t *testing.T) {
	c := nativeClient(t)
	nativeAdd(t, c, "192.0.2.100/32")
	nativeAdd(t, c, "192.0.2.101/32")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := c.DeletePath(ctx, &api.DeletePathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Family: ipv4()}); err != nil {
		t.Fatal(err)
	}
	if paths := nativeList(t, c); len(paths) != 0 {
		t.Fatalf("native broad-delete contract changed: %+v", paths)
	}
}
