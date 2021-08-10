package grpc

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/require"

	"github.com/hashicorp/consul/agent/grpc/internal/testservice"
	"github.com/hashicorp/consul/agent/grpc/resolver"
	"github.com/hashicorp/consul/agent/metadata"
	"github.com/hashicorp/consul/sdk/freeport"
	"github.com/hashicorp/consul/tlsutil"
)

// TODO(rb): add tests for the wanfed/alpn variations

// useTLSForDcAlwaysTrue tell GRPC to always return the TLS is enabled
func useTLSForDcAlwaysTrue(_ string) bool {
	return true
}

func TestNewDialer_WithTLSWrapper(t *testing.T) {
	ports := freeport.MustTake(1)
	defer freeport.Return(ports)

	lis, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[0])))
	require.NoError(t, err)
	t.Cleanup(logError(t, lis.Close))

	builder, err := resolver.NewServerResolverBuilder(resolver.Config{})
	require.NoError(t, err)
	builder.AddServer(&metadata.Server{
		Name:       "server-1",
		ID:         "ID1",
		Datacenter: "dc1",
		Addr:       lis.Addr(),
		UseTLS:     true,
	})

	var called bool
	wrapper := func(_ string, conn net.Conn) (net.Conn, error) {
		called = true
		return conn, nil
	}
	dial := newDialer(
		builder,
		nil,
		nil,
		wrapper,
		nil,
		useTLSForDcAlwaysTrue,
		true,
		"dc1",
	)
	ctx := context.Background()
	conn, err := dial(ctx, resolver.DCPrefix("dc1", lis.Addr().String()))
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	require.True(t, called, "expected TLSWrapper to be called")
}

func TestNewDialer_IntegrationWithTLSEnabledHandler(t *testing.T) {
	res, err := resolver.NewServerResolverBuilder(newConfig(t))
	require.NoError(t, err)
	registerWithGRPC(t, res)

	srv := newTestServer(t, "server-1", "dc1")
	tlsConf, err := tlsutil.NewConfigurator(tlsutil.Config{
		VerifyIncoming: true,
		VerifyOutgoing: true,
		CAFile:         "../../test/hostname/CertAuth.crt",
		CertFile:       "../../test/hostname/Alice.crt",
		KeyFile:        "../../test/hostname/Alice.key",
	}, hclog.New(nil))
	require.NoError(t, err)
	srv.rpc.tlsConf = tlsConf

	md := srv.Metadata()
	res.AddServer(md)
	t.Cleanup(srv.shutdown)

	pool := NewClientConnPool(
		res,
		nil,
		TLSWrapper(tlsConf.OutgoingRPCWrapper()),
		nil,
		tlsConf.UseTLS,
		true,
		"dc1",
	)

	conn, err := pool.ClientConn("dc1")
	require.NoError(t, err)
	client := testservice.NewSimpleClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)

	resp, err := client.Something(ctx, &testservice.Req{})
	require.NoError(t, err)
	require.Equal(t, "server-1", resp.ServerName)
	require.True(t, atomic.LoadInt32(&srv.rpc.tlsConnEstablished) > 0)
}

func TestClientConnPool_IntegrationWithGRPCResolver_Failover(t *testing.T) {
	count := 4
	res, err := resolver.NewServerResolverBuilder(newConfig(t))
	require.NoError(t, err)
	registerWithGRPC(t, res)
	pool := NewClientConnPool(
		res,
		nil,
		nil,
		nil,
		useTLSForDcAlwaysTrue,
		true,
		"dc1",
	)

	for i := 0; i < count; i++ {
		name := fmt.Sprintf("server-%d", i)
		srv := newTestServer(t, name, "dc1")
		res.AddServer(srv.Metadata())
		t.Cleanup(srv.shutdown)
	}

	conn, err := pool.ClientConn("dc1")
	require.NoError(t, err)
	client := testservice.NewSimpleClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)

	first, err := client.Something(ctx, &testservice.Req{})
	require.NoError(t, err)

	res.RemoveServer(&metadata.Server{ID: first.ServerName, Datacenter: "dc1"})

	resp, err := client.Something(ctx, &testservice.Req{})
	require.NoError(t, err)
	require.NotEqual(t, resp.ServerName, first.ServerName)
}

func TestClientConnPool_ForwardToLeader_Failover(t *testing.T) {
	count := 3
	conf := newConfig(t)
	res, err := resolver.NewServerResolverBuilder(conf)
	require.NoError(t, err)
	registerWithGRPC(t, res)
	pool := NewClientConnPool(
		res,
		nil,
		nil,
		nil,
		useTLSForDcAlwaysTrue,
		true,
		"dc1",
	)

	var servers []testServer
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("server-%d", i)
		srv := newTestServer(t, name, "dc1")
		res.AddServer(srv.Metadata())
		servers = append(servers, srv)
		t.Cleanup(srv.shutdown)
	}

	// Set the leader address to the first server.
	srv0 := servers[0].Metadata()
	res.UpdateLeaderAddr(srv0.Datacenter, srv0.Addr.String())

	conn, err := pool.ClientConnLeader()
	require.NoError(t, err)
	client := testservice.NewSimpleClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)

	first, err := client.Something(ctx, &testservice.Req{})
	require.NoError(t, err)
	require.Equal(t, first.ServerName, servers[0].name)

	// Update the leader address and make another request.
	srv1 := servers[1].Metadata()
	res.UpdateLeaderAddr(srv1.Datacenter, srv1.Addr.String())

	resp, err := client.Something(ctx, &testservice.Req{})
	require.NoError(t, err)
	require.Equal(t, resp.ServerName, servers[1].name)
}

func newConfig(t *testing.T) resolver.Config {
	n := t.Name()
	s := strings.Replace(n, "/", "", -1)
	s = strings.Replace(s, "_", "", -1)
	return resolver.Config{Authority: strings.ToLower(s)}
}

func TestClientConnPool_IntegrationWithGRPCResolver_Rebalance(t *testing.T) {
	count := 5
	res, err := resolver.NewServerResolverBuilder(newConfig(t))
	require.NoError(t, err)
	registerWithGRPC(t, res)
	pool := NewClientConnPool(
		res,
		nil,
		nil,
		nil,
		useTLSForDcAlwaysTrue,
		true,
		"dc1",
	)

	for i := 0; i < count; i++ {
		name := fmt.Sprintf("server-%d", i)
		srv := newTestServer(t, name, "dc1")
		res.AddServer(srv.Metadata())
		t.Cleanup(srv.shutdown)
	}

	conn, err := pool.ClientConn("dc1")
	require.NoError(t, err)
	client := testservice.NewSimpleClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)

	first, err := client.Something(ctx, &testservice.Req{})
	require.NoError(t, err)

	t.Run("rebalance a different DC, does nothing", func(t *testing.T) {
		res.NewRebalancer("dc-other")()

		resp, err := client.Something(ctx, &testservice.Req{})
		require.NoError(t, err)
		require.Equal(t, resp.ServerName, first.ServerName)
	})

	t.Run("rebalance the dc", func(t *testing.T) {
		// Rebalance is random, but if we repeat it a few times it should give us a
		// new server.
		attempts := 100
		for i := 0; i < attempts; i++ {
			res.NewRebalancer("dc1")()

			resp, err := client.Something(ctx, &testservice.Req{})
			require.NoError(t, err)
			if resp.ServerName != first.ServerName {
				return
			}
		}
		t.Fatalf("server was not rebalanced after %v attempts", attempts)
	})
}

func TestClientConnPool_IntegrationWithGRPCResolver_MultiDC(t *testing.T) {
	dcs := []string{"dc1", "dc2", "dc3"}

	res, err := resolver.NewServerResolverBuilder(newConfig(t))
	require.NoError(t, err)
	registerWithGRPC(t, res)
	pool := NewClientConnPool(
		res,
		nil,
		nil,
		nil,
		useTLSForDcAlwaysTrue,
		true,
		"dc1",
	)

	for _, dc := range dcs {
		name := "server-0-" + dc
		srv := newTestServer(t, name, dc)
		res.AddServer(srv.Metadata())
		t.Cleanup(srv.shutdown)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)

	for _, dc := range dcs {
		conn, err := pool.ClientConn(dc)
		require.NoError(t, err)
		client := testservice.NewSimpleClient(conn)

		resp, err := client.Something(ctx, &testservice.Req{})
		require.NoError(t, err)
		require.Equal(t, resp.Datacenter, dc)
	}
}

func registerWithGRPC(t *testing.T, b *resolver.ServerResolverBuilder) {
	resolver.Register(b)
	t.Cleanup(func() {
		resolver.Deregister(b.Authority())
	})
}
