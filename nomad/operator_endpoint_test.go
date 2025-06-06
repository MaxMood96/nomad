// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package nomad

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/rpc"
	"os"
	"path"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-msgpack/v2/codec"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc/v2"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/ci"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/helper/snapshot"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/hashicorp/raft"
	"github.com/shoenig/test/must"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	// RPC Permission Denied Errors - currently `rpc error: Permission denied`
	rpcPermDeniedErr = rpc.ServerError(structs.ErrPermissionDenied.Error())
)

func TestOperator_RaftGetConfiguration(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, nil)
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)

	arg := structs.GenericRequest{
		QueryOptions: structs.QueryOptions{
			Region: s1.config.Region,
		},
	}
	var reply structs.RaftConfigurationResponse
	if err := msgpackrpc.CallWithCodec(codec, "Operator.RaftGetConfiguration", &arg, &reply); err != nil {
		t.Fatalf("err: %v", err)
	}

	future := s1.raft.GetConfiguration()
	if err := future.Error(); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(future.Configuration().Servers) != 1 {
		t.Fatalf("bad: %v", future.Configuration().Servers)
	}
	me := future.Configuration().Servers[0]
	expected := structs.RaftConfigurationResponse{
		Servers: []*structs.RaftServer{
			{
				ID:           me.ID,
				Node:         fmt.Sprintf("%v.%v", s1.config.NodeName, s1.config.Region),
				Address:      me.Address,
				Leader:       true,
				Voter:        true,
				RaftProtocol: fmt.Sprintf("%d", s1.config.RaftConfig.ProtocolVersion),
			},
		},
		Index: future.Index(),
	}
	if !reflect.DeepEqual(reply, expected) {
		t.Fatalf("bad: got %+v; want %+v", reply, expected)
	}
}

func TestOperator_RaftGetConfiguration_ACL(t *testing.T) {
	ci.Parallel(t)

	s1, root, cleanupS1 := TestACLServer(t, nil)
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)
	assert := assert.New(t)
	state := s1.fsm.State()

	// Create ACL token
	invalidToken := mock.CreatePolicyAndToken(t, state, 1001, "test-invalid", mock.NodePolicy(acl.PolicyWrite))

	arg := structs.GenericRequest{
		QueryOptions: structs.QueryOptions{
			Region: s1.config.Region,
		},
	}

	// Try with no token and expect permission denied
	{
		var reply structs.RaftConfigurationResponse
		err := msgpackrpc.CallWithCodec(codec, "Operator.RaftGetConfiguration", &arg, &reply)
		assert.NotNil(err)
		assert.Equal(err.Error(), structs.ErrPermissionDenied.Error())
	}

	// Try with an invalid token and expect permission denied
	{
		arg.AuthToken = invalidToken.SecretID
		var reply structs.RaftConfigurationResponse
		err := msgpackrpc.CallWithCodec(codec, "Operator.RaftGetConfiguration", &arg, &reply)
		assert.NotNil(err)
		assert.Equal(err.Error(), structs.ErrPermissionDenied.Error())
	}

	// Use management token
	{
		arg.AuthToken = root.SecretID
		var reply structs.RaftConfigurationResponse
		assert.Nil(msgpackrpc.CallWithCodec(codec, "Operator.RaftGetConfiguration", &arg, &reply))

		future := s1.raft.GetConfiguration()
		assert.Nil(future.Error())
		assert.Len(future.Configuration().Servers, 1)

		me := future.Configuration().Servers[0]
		expected := structs.RaftConfigurationResponse{
			Servers: []*structs.RaftServer{
				{
					ID:           me.ID,
					Node:         fmt.Sprintf("%v.%v", s1.config.NodeName, s1.config.Region),
					Address:      me.Address,
					Leader:       true,
					Voter:        true,
					RaftProtocol: fmt.Sprintf("%d", s1.config.RaftConfig.ProtocolVersion),
				},
			},
			Index: future.Index(),
		}
		assert.Equal(expected, reply)
	}
}

func TestOperator_RaftRemovePeerByID(t *testing.T) {
	ci.Parallel(t)

	s1, root, cleanupS1 := TestACLServer(t, nil)
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)
	store := s1.fsm.State()

	var reply struct{}

	// Try to remove a peer that's not there.
	arg := structs.RaftPeerByIDRequest{
		ID: raft.ServerID(uuid.Generate()),
		WriteRequest: structs.WriteRequest{
			Region: s1.config.Region, AuthToken: root.SecretID},
	}
	err := msgpackrpc.CallWithCodec(codec, "Operator.RaftRemovePeerByID", &arg, &reply)
	must.ErrorContains(t, err, "not found in the Raft configuration")

	// Create ACL token
	invalidToken := mock.CreatePolicyAndToken(t,
		store, 1001, "test-invalid", mock.NodePolicy(acl.PolicyWrite))

	arg = structs.RaftPeerByIDRequest{
		ID: raft.ServerID("e35bde83-4e9c-434f-a6ef-453f44ee21ea"),
	}
	arg.Region = s1.config.Region

	ports := ci.PortAllocator.Grab(1)

	// Add peer manually to Raft.
	{
		future := s1.raft.AddVoter(arg.ID,
			raft.ServerAddress(fmt.Sprintf("127.0.0.1:%d", ports[0])), 0, 0)
		must.NoError(t, future.Error())
	}

	// Make sure it's there.
	{
		future := s1.raft.GetConfiguration()
		err := future.Error()
		must.NoError(t, err)

		configuration := future.Configuration()
		must.Len(t, 2, configuration.Servers)
	}

	// Try with no token and expect permission denied
	{
		err := msgpackrpc.CallWithCodec(codec, "Operator.RaftRemovePeerByID", &arg, &reply)
		must.EqError(t, err, structs.ErrPermissionDenied.Error())
	}

	// Try with an invalid token and expect permission denied
	{
		arg.AuthToken = invalidToken.SecretID
		err := msgpackrpc.CallWithCodec(codec, "Operator.RaftRemovePeerByID", &arg, &reply)
		must.EqError(t, err, structs.ErrPermissionDenied.Error())
	}

	// Try with a management token
	{
		arg.AuthToken = root.SecretID
		err := msgpackrpc.CallWithCodec(codec, "Operator.RaftRemovePeerByID", &arg, &reply)
		must.NoError(t, err)
	}

	// Make sure it's removed.
	{
		future := s1.raft.GetConfiguration()
		err := future.Error()
		must.NoError(t, err)

		configuration := future.Configuration()
		must.Len(t, 1, configuration.Servers)
	}
}

type testcluster struct {
	t       *testing.T
	args    tcArgs
	server  []*Server
	cleanup []func()
	token   *structs.ACLToken
}

func (tc testcluster) Cleanup() {
	for _, cFn := range tc.cleanup {
		cFn()
	}
}

type tcArgs struct {
	size      int
	enableACL bool
}

func newTestCluster(t *testing.T, args tcArgs) (tc testcluster) {
	// handle the zero case reasonably for count
	if args.size == 0 {
		args.size = 3
	}
	if args.size < 1 {
		t.Fatal("newTestCluster must have size greater than zero")
	}
	cSize := args.size
	out := testcluster{
		t:       t,
		args:    args,
		server:  make([]*Server, cSize),
		cleanup: make([]func(), cSize),
	}

	for i := 0; i < cSize; i += 1 {
		out.server[i], out.cleanup[i] = TestServer(t, func(c *Config) {
			c.NodeName = fmt.Sprintf("node-%v", i+1)
			c.RaftConfig.ProtocolVersion = raft.ProtocolVersion(3)
			c.BootstrapExpect = cSize
			c.ACLEnabled = args.enableACL
		})
	}
	t.Cleanup(out.Cleanup)

	TestJoin(t, out.server...)
	out.WaitForLeader()

	if args.enableACL {
		s1 := out.server[0]
		bsToken := new(structs.ACLToken)
		// Bootstrap the ACL subsystem
		req := &structs.ACLTokenBootstrapRequest{
			Token:        bsToken,
			WriteRequest: structs.WriteRequest{Region: s1.config.Region},
		}
		resp := &structs.ACLTokenUpsertResponse{}
		err := out.server[0].RPC("ACL.Bootstrap", req, resp)
		if err != nil {
			t.Fatalf("failed to bootstrap ACL token: %v", err)
		}
		t.Logf("bootstrap token: %v", *resp.Tokens[0])
		out.token = resp.Tokens[0]
	}
	return out
}

// WaitForLeader performs a parallel WaitForLeader over each cluster member,
// because testutil doesn't export rpcFn so we can't create a collection of
// rpcFn to use testutil.WaitForLeaders directly.
func (tc testcluster) WaitForLeader() {
	var wg sync.WaitGroup
	for i := 0; i < len(tc.server); i++ {
		idx := i
		wg.Add(1)
		go func() {
			defer wg.Done()

			// The WaitForLeader func uses WaitForResultRetries
			// so this should timeout at 5 seconds * test multiplier
			testutil.WaitForLeader(tc.t, tc.server[idx].RPC)
		}()
	}
	wg.Wait()
}

func (tc testcluster) leader() *Server {
	tc.WaitForLeader()
	for _, s := range tc.server {
		if isLeader, _ := s.getLeader(); isLeader {
			return s
		}
	}
	return nil
}
func (tc testcluster) anyFollowerRaftServerID() raft.ServerID {
	tc.WaitForLeader()
	s1 := tc.server[0]
	_, ldrID := s1.raft.LeaderWithID()

	var tgtID raft.ServerID

	s1.peerLock.Lock()
	defer s1.peerLock.Unlock()

	// Find the first non-leader server in the list.
	for _, sp := range s1.localPeers {
		tgtID = raft.ServerID(sp.ID)
		if tgtID != ldrID {
			break
		}
	}
	return tgtID
}
func (tc testcluster) anyFollowerRaftServerAddress() raft.ServerAddress {
	tc.WaitForLeader()
	s1 := tc.server[0]
	lAddr, _ := s1.raft.LeaderWithID()

	var addr raft.ServerAddress

	s1.peerLock.Lock()
	defer s1.peerLock.Unlock()

	// Find the first non-leader server in the list.
	for a := range s1.localPeers {
		addr = a
		if addr != lAddr {
			break
		}
	}
	return addr
}

func TestOperator_TransferLeadershipToServerAddress_ACL(t *testing.T) {
	ci.Parallel(t)
	var err error

	tc := newTestCluster(t, tcArgs{enableACL: true})
	s1 := tc.leader()
	must.NotNil(t, s1)
	codec := rpcClient(t, s1)

	addr := tc.anyFollowerRaftServerAddress()

	mgmtWR := structs.WriteRequest{
		Region:    s1.config.Region,
		AuthToken: tc.token.SecretID,
	}

	// Create invalid ACL Token
	pReq := &structs.ACLPolicyUpsertRequest{
		Policies: []*structs.ACLPolicy{
			{
				Name:  "node-write-only",
				Rules: `node { policy = "write" }`,
			},
		},
		WriteRequest: mgmtWR,
	}
	pResp := &structs.GenericResponse{}
	err = msgpackrpc.CallWithCodec(codec, structs.ACLUpsertPoliciesRPCMethod, pReq, pResp)
	must.NoError(t, err)

	tReq := &structs.ACLTokenUpsertRequest{
		Tokens: []*structs.ACLToken{
			{
				Name:     "invalid",
				Policies: []string{"node_write_only"},
				Type:     structs.ACLClientToken,
			},
		},
		WriteRequest: mgmtWR,
	}
	tResp := &structs.ACLTokenUpsertResponse{}
	err = msgpackrpc.CallWithCodec(codec, structs.ACLUpsertTokensRPCMethod, tReq, tResp)
	must.NoError(t, err)

	invalidToken := tResp.Tokens[0]

	testReq := &structs.RaftPeerRequest{
		RaftIDAddress: structs.RaftIDAddress{Address: addr},
		WriteRequest: structs.WriteRequest{
			Region: s1.config.Region,
		},
	}

	var reply struct{}

	t.Run("no-token", func(t *testing.T) {
		// Try with no token and expect permission denied
		err := msgpackrpc.CallWithCodec(codec, "Operator.TransferLeadershipToPeer", testReq, &reply)
		must.Error(t, err)
		must.ErrorIs(t, err, rpcPermDeniedErr)
	})

	t.Run("invalid-token", func(t *testing.T) {
		// Try with an invalid token and expect permission denied
		testReq.AuthToken = invalidToken.SecretID
		err := msgpackrpc.CallWithCodec(codec, "Operator.TransferLeadershipToPeer", testReq, &reply)
		must.Error(t, err)
		must.ErrorIs(t, err, rpcPermDeniedErr)
	})

	t.Run("good-token", func(t *testing.T) {
		// Try with a management token
		testReq.AuthToken = tc.token.SecretID
		err := msgpackrpc.CallWithCodec(codec, "Operator.TransferLeadershipToPeer", testReq, &reply)
		must.NoError(t, err)

		// Is the expected leader the new one?
		tc.WaitForLeader()
		lAddrNew, _ := s1.raft.LeaderWithID()
		must.Eq(t, addr, lAddrNew)
	})
}

func TestOperator_TransferLeadershipToServerID_ACL(t *testing.T) {
	ci.Parallel(t)
	var err error

	tc := newTestCluster(t, tcArgs{enableACL: true})
	s1 := tc.leader()
	must.NotNil(t, s1)
	codec := rpcClient(t, s1)

	mgmtWR := structs.WriteRequest{
		Region:    s1.config.Region,
		AuthToken: tc.token.SecretID,
	}

	// Create invalid ACL Token
	pReq := &structs.ACLPolicyUpsertRequest{
		Policies: []*structs.ACLPolicy{
			{
				Name:  "node-write-only",
				Rules: `node { policy = "write" }`,
			},
		},
		WriteRequest: mgmtWR,
	}
	pResp := &structs.GenericResponse{}
	err = msgpackrpc.CallWithCodec(codec, structs.ACLUpsertPoliciesRPCMethod, pReq, pResp)
	must.NoError(t, err)

	tReq := &structs.ACLTokenUpsertRequest{
		Tokens: []*structs.ACLToken{
			{
				Name:     "invalid",
				Policies: []string{"node_write_only"},
				Type:     structs.ACLClientToken,
			},
		},
		WriteRequest: mgmtWR,
	}
	tResp := &structs.ACLTokenUpsertResponse{}
	err = msgpackrpc.CallWithCodec(codec, structs.ACLUpsertTokensRPCMethod, tReq, tResp)
	must.NoError(t, err)

	invalidToken := tResp.Tokens[0]

	tgtID := tc.anyFollowerRaftServerID()
	testReq := &structs.RaftPeerRequest{
		RaftIDAddress: structs.RaftIDAddress{
			ID: tgtID,
		},
		WriteRequest: structs.WriteRequest{Region: s1.config.Region},
	}
	var reply struct{}

	t.Run("no-token", func(t *testing.T) {
		// Try with no token and expect permission denied
		err := msgpackrpc.CallWithCodec(codec, "Operator.TransferLeadershipToPeer", testReq, &reply)
		must.Error(t, err)
		must.ErrorIs(t, err, rpcPermDeniedErr)
	})

	t.Run("invalid-token", func(t *testing.T) {
		// Try with an invalid token and expect permission denied
		testReq.AuthToken = invalidToken.SecretID
		err := msgpackrpc.CallWithCodec(codec, "Operator.TransferLeadershipToPeer", testReq, &reply)
		must.Error(t, err)
		must.ErrorIs(t, err, rpcPermDeniedErr)
	})

	t.Run("good-token", func(t *testing.T) {
		// Try with a management token
		testReq.AuthToken = tc.token.SecretID
		err := msgpackrpc.CallWithCodec(codec, "Operator.TransferLeadershipToPeer", testReq, &reply)
		must.NoError(t, err)

		// Is the expected leader the new one?
		tc.WaitForLeader()
		_, ldrID := s1.raft.LeaderWithID()
		must.Eq(t, tgtID, ldrID)
	})
}

func TestOperator_SchedulerGetConfiguration(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, func(c *Config) {
		c.Build = "1.3.0+unittest"
	})
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)

	arg := structs.GenericRequest{
		QueryOptions: structs.QueryOptions{
			Region: s1.config.Region,
		},
	}
	var reply structs.SchedulerConfigurationResponse
	if err := msgpackrpc.CallWithCodec(codec, "Operator.SchedulerGetConfiguration", &arg, &reply); err != nil {
		t.Fatalf("err: %v", err)
	}
	require := require.New(t)
	require.NotZero(reply.Index)
	require.True(reply.SchedulerConfig.PreemptionConfig.SystemSchedulerEnabled)
}

func TestOperator_SchedulerSetConfiguration(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, func(c *Config) {
		c.Build = "1.3.0+unittest"
	})
	defer cleanupS1()
	rpcCodec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)

	// Disable preemption and pause the eval broker.
	arg := structs.SchedulerSetConfigRequest{
		Config: structs.SchedulerConfiguration{
			PreemptionConfig: structs.PreemptionConfig{
				SystemSchedulerEnabled: false,
			},
			PauseEvalBroker: true,
		},
	}
	arg.Region = s1.config.Region

	var setResponse structs.SchedulerSetConfigurationResponse
	err := msgpackrpc.CallWithCodec(rpcCodec, "Operator.SchedulerSetConfiguration", &arg, &setResponse)
	require.Nil(t, err)
	require.NotZero(t, setResponse.Index)

	// Read and verify that preemption is disabled and the eval and blocked
	// evals systems are disabled.
	readConfig := structs.GenericRequest{
		QueryOptions: structs.QueryOptions{
			Region: s1.config.Region,
		},
	}
	var reply structs.SchedulerConfigurationResponse
	err = msgpackrpc.CallWithCodec(rpcCodec, "Operator.SchedulerGetConfiguration", &readConfig, &reply)
	require.NoError(t, err)

	require.NotZero(t, reply.Index)
	require.False(t, reply.SchedulerConfig.PreemptionConfig.SystemSchedulerEnabled)
	require.True(t, reply.SchedulerConfig.PauseEvalBroker)

	require.False(t, s1.evalBroker.Enabled())
	require.False(t, s1.blockedEvals.Enabled())
}

func TestOperator_SchedulerGetConfiguration_ACL(t *testing.T) {
	ci.Parallel(t)

	s1, root, cleanupS1 := TestACLServer(t, func(c *Config) {
		c.RaftConfig.ProtocolVersion = 3
		c.Build = "1.3.0+unittest"
	})
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)
	state := s1.fsm.State()

	// Create ACL token
	invalidToken := mock.CreatePolicyAndToken(t, state, 1001, "test-invalid", mock.NodePolicy(acl.PolicyWrite))

	arg := structs.GenericRequest{
		QueryOptions: structs.QueryOptions{
			Region: s1.config.Region,
		},
	}
	require := require.New(t)
	var reply structs.SchedulerConfigurationResponse

	// Try with no token and expect permission denied
	{
		err := msgpackrpc.CallWithCodec(codec, "Operator.SchedulerGetConfiguration", &arg, &reply)
		require.NotNil(err)
		require.Equal(err.Error(), structs.ErrPermissionDenied.Error())
	}

	// Try with an invalid token and expect permission denied
	{
		arg.AuthToken = invalidToken.SecretID
		err := msgpackrpc.CallWithCodec(codec, "Operator.SchedulerGetConfiguration", &arg, &reply)
		require.NotNil(err)
		require.Equal(err.Error(), structs.ErrPermissionDenied.Error())
	}

	// Try with root token, should succeed
	{
		arg.AuthToken = root.SecretID
		err := msgpackrpc.CallWithCodec(codec, "Operator.SchedulerGetConfiguration", &arg, &reply)
		require.Nil(err)
	}

}

func TestOperator_SchedulerSetConfiguration_ACL(t *testing.T) {
	ci.Parallel(t)

	s1, root, cleanupS1 := TestACLServer(t, func(c *Config) {
		c.RaftConfig.ProtocolVersion = 3
		c.Build = "1.3.0+unittest"
	})
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)
	state := s1.fsm.State()

	// Create ACL token
	invalidToken := mock.CreatePolicyAndToken(t, state, 1001, "test-invalid", mock.NodePolicy(acl.PolicyWrite))

	arg := structs.SchedulerSetConfigRequest{
		Config: structs.SchedulerConfiguration{
			PreemptionConfig: structs.PreemptionConfig{
				SystemSchedulerEnabled: true,
			},
		},
	}
	arg.Region = s1.config.Region

	require := require.New(t)
	var reply structs.SchedulerSetConfigurationResponse

	// Try with no token and expect permission denied
	{
		err := msgpackrpc.CallWithCodec(codec, "Operator.SchedulerSetConfiguration", &arg, &reply)
		require.NotNil(err)
		require.Equal(structs.ErrPermissionDenied.Error(), err.Error())
	}

	// Try with an invalid token and expect permission denied
	{
		arg.AuthToken = invalidToken.SecretID
		err := msgpackrpc.CallWithCodec(codec, "Operator.SchedulerSetConfiguration", &arg, &reply)
		require.NotNil(err)
		require.Equal(err.Error(), structs.ErrPermissionDenied.Error())
	}

	// Try with root token, should succeed
	{
		arg.AuthToken = root.SecretID
		err := msgpackrpc.CallWithCodec(codec, "Operator.SchedulerSetConfiguration", &arg, &reply)
		require.Nil(err)
	}

}

func TestOperator_SnapshotSave(t *testing.T) {
	ci.Parallel(t)

	////// Nomad clusters topology - not specific to test
	dir := t.TempDir()

	server1, cleanupLS := TestServer(t, func(c *Config) {
		c.BootstrapExpect = 2
		c.DevMode = false
		c.DataDir = path.Join(dir, "server1")
	})
	defer cleanupLS()

	server2, cleanupRS := TestServer(t, func(c *Config) {
		c.BootstrapExpect = 2
		c.DevMode = false
		c.DataDir = path.Join(dir, "server2")
	})
	defer cleanupRS()

	remoteRegionServer, cleanupRRS := TestServer(t, func(c *Config) {
		c.Region = "two"
		c.DevMode = false
		c.DataDir = path.Join(dir, "remote_region_server")
	})
	defer cleanupRRS()

	TestJoin(t, server1, server2)
	TestJoin(t, server1, remoteRegionServer)
	testutil.WaitForLeader(t, server1.RPC)
	testutil.WaitForLeader(t, server2.RPC)
	testutil.WaitForLeader(t, remoteRegionServer.RPC)

	leader, nonLeader := server1, server2
	if server2.IsLeader() {
		leader, nonLeader = server2, server1
	}

	/////////  Actually run query now
	cases := []struct {
		name   string
		server *Server
	}{
		{"leader", leader},
		{"non_leader", nonLeader},
		{"remote_region", remoteRegionServer},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			handler, err := c.server.StreamingRpcHandler("Operator.SnapshotSave")
			require.NoError(t, err)

			p1, p2 := net.Pipe()
			defer p1.Close()
			defer p2.Close()

			// start handler
			go handler(p2)

			var req structs.SnapshotSaveRequest
			var resp structs.SnapshotSaveResponse

			req.Region = "global"

			// send request
			encoder := codec.NewEncoder(p1, structs.MsgpackHandle)
			err = encoder.Encode(&req)
			require.NoError(t, err)

			decoder := codec.NewDecoder(p1, structs.MsgpackHandle)
			err = decoder.Decode(&resp)
			require.NoError(t, err)
			require.Empty(t, resp.ErrorMsg)

			require.NotZero(t, resp.Index)
			require.NotEmpty(t, resp.SnapshotChecksum)
			require.Contains(t, resp.SnapshotChecksum, "sha-256=")

			index := resp.Index

			snap, err := os.CreateTemp("", "nomadtests-snapshot-")
			require.NoError(t, err)
			defer os.Remove(snap.Name())

			hash := sha256.New()
			_, err = io.Copy(io.MultiWriter(snap, hash), p1)
			require.NoError(t, err)

			expectedChecksum := "sha-256=" + base64.StdEncoding.EncodeToString(hash.Sum(nil))

			require.Equal(t, expectedChecksum, resp.SnapshotChecksum)

			_, err = snap.Seek(0, 0)
			require.NoError(t, err)

			meta, err := snapshot.Verify(snap)
			require.NoError(t, err)

			require.NotZerof(t, meta.Term, "snapshot term")
			require.Equal(t, index, meta.Index)
		})
	}
}

func TestOperator_SnapshotSave_ACL(t *testing.T) {
	ci.Parallel(t)

	////// Nomad clusters topology - not specific to test
	dir := t.TempDir()

	s, root, cleanupLS := TestACLServer(t, func(c *Config) {
		c.BootstrapExpect = 1
		c.DevMode = false
		c.DataDir = path.Join(dir, "server1")
	})
	defer cleanupLS()

	testutil.WaitForLeader(t, s.RPC)

	deniedToken := mock.CreatePolicyAndToken(t, s.fsm.State(), 1001, "test-invalid", mock.NodePolicy(acl.PolicyWrite))

	/////////  Actually run query now
	cases := []struct {
		name    string
		token   string
		errCode int
		err     error
	}{
		{"root", root.SecretID, 0, nil},
		{"no_permission_token", deniedToken.SecretID, 403, structs.ErrPermissionDenied},
		{"invalid token", uuid.Generate(), 403, structs.ErrPermissionDenied},
		{"unauthenticated", "", 403, structs.ErrPermissionDenied},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			handler, err := s.StreamingRpcHandler("Operator.SnapshotSave")
			require.NoError(t, err)

			p1, p2 := net.Pipe()
			defer p1.Close()
			defer p2.Close()

			// start handler
			go handler(p2)

			var req structs.SnapshotSaveRequest
			var resp structs.SnapshotSaveResponse

			req.Region = "global"
			req.AuthToken = c.token

			// send request
			encoder := codec.NewEncoder(p1, structs.MsgpackHandle)
			err = encoder.Encode(&req)
			require.NoError(t, err)

			decoder := codec.NewDecoder(p1, structs.MsgpackHandle)
			err = decoder.Decode(&resp)
			require.NoError(t, err)

			// streaming errors appear as a response rather than a returned error
			if c.err != nil {
				require.Equal(t, c.err.Error(), resp.ErrorMsg)
				require.Equal(t, c.errCode, resp.ErrorCode)
				return

			}

			require.NotZero(t, resp.Index)
			require.NotEmpty(t, resp.SnapshotChecksum)
			require.Contains(t, resp.SnapshotChecksum, "sha-256=")

			io.Copy(io.Discard, p1)
		})
	}
}

func TestOperator_SnapshotRestore(t *testing.T) {
	ci.Parallel(t)

	targets := []string{"leader", "non_leader", "remote_region"}

	for _, c := range targets {
		t.Run(c, func(t *testing.T) {
			snap, job := generateSnapshot(t)

			checkFn := func(t *testing.T, s *Server) {
				found, err := s.State().JobByID(nil, job.Namespace, job.ID)
				require.NoError(t, err)
				require.Equal(t, job.ID, found.ID)
			}

			var req structs.SnapshotRestoreRequest
			req.Region = "global"
			testRestoreSnapshot(t, &req, snap, c, checkFn)
		})
	}
}

func generateSnapshot(t *testing.T) (*snapshot.Snapshot, *structs.Job) {
	dir := t.TempDir()

	s, cleanup := TestServer(t, func(c *Config) {
		c.BootstrapExpect = 1
		c.DevMode = false
		c.DataDir = path.Join(dir, "server1")
	})
	defer cleanup()

	job := mock.Job()
	jobReq := &structs.JobRegisterRequest{
		Job: job,
		WriteRequest: structs.WriteRequest{
			Region:    "global",
			Namespace: job.Namespace,
		},
	}
	var jobResp structs.JobRegisterResponse
	codec := rpcClient(t, s)
	err := msgpackrpc.CallWithCodec(codec, "Job.Register", jobReq, &jobResp)
	require.NoError(t, err)

	err = s.State().UpsertJob(structs.MsgTypeTestSetup, 1000, nil, job)
	require.NoError(t, err)

	snapshot, err := snapshot.New(s.logger, s.raft)
	require.NoError(t, err)

	t.Cleanup(func() { snapshot.Close() })

	return snapshot, job
}

func testRestoreSnapshot(t *testing.T, req *structs.SnapshotRestoreRequest, snapshot io.Reader, target string,
	assertionFn func(t *testing.T, server *Server)) {

	////// Nomad clusters topology - not specific to test
	dir := t.TempDir()

	server1, cleanupLS := TestServer(t, func(c *Config) {
		c.BootstrapExpect = 2
		c.DevMode = false
		c.DataDir = path.Join(dir, "server1")

		// increase times outs to account for I/O operations that
		// snapshot restore performs - some of which require sync calls
		c.RaftConfig.LeaderLeaseTimeout = 1 * time.Second
		c.RaftConfig.HeartbeatTimeout = 1 * time.Second
		c.RaftConfig.ElectionTimeout = 1 * time.Second
		c.RaftTimeout = 5 * time.Second
	})
	defer cleanupLS()

	server2, cleanupRS := TestServer(t, func(c *Config) {
		c.BootstrapExpect = 2
		c.DevMode = false
		c.DataDir = path.Join(dir, "server2")

		// increase times outs to account for I/O operations that
		// snapshot restore performs - some of which require sync calls
		c.RaftConfig.LeaderLeaseTimeout = 1 * time.Second
		c.RaftConfig.HeartbeatTimeout = 1 * time.Second
		c.RaftConfig.ElectionTimeout = 1 * time.Second
		c.RaftTimeout = 5 * time.Second
	})
	defer cleanupRS()

	remoteRegionServer, cleanupRRS := TestServer(t, func(c *Config) {
		c.Region = "two"
		c.DevMode = false
		c.DataDir = path.Join(dir, "remote_region_server")
	})
	defer cleanupRRS()

	TestJoin(t, server1, server2)
	TestJoin(t, server1, remoteRegionServer)
	testutil.WaitForLeader(t, server1.RPC)
	testutil.WaitForLeader(t, server2.RPC)
	testutil.WaitForLeader(t, remoteRegionServer.RPC)

	leader, nonLeader := server1, server2
	if server2.IsLeader() {
		leader, nonLeader = server2, server1
	}

	/////////  Actually run query now
	mapping := map[string]*Server{
		"leader":        leader,
		"non_leader":    nonLeader,
		"remote_region": remoteRegionServer,
	}

	server := mapping[target]
	require.NotNil(t, server, "target not found")

	handler, err := server.StreamingRpcHandler("Operator.SnapshotRestore")
	require.NoError(t, err)

	p1, p2 := net.Pipe()
	defer p1.Close()
	defer p2.Close()

	// start handler
	go handler(p2)

	var resp structs.SnapshotRestoreResponse

	// send request
	encoder := codec.NewEncoder(p1, structs.MsgpackHandle)
	err = encoder.Encode(req)
	require.NoError(t, err)

	buf := make([]byte, 1024)
	for {
		n, err := snapshot.Read(buf)
		if n > 0 {
			require.NoError(t, encoder.Encode(&cstructs.StreamErrWrapper{Payload: buf[:n]}))
		}
		if err != nil {
			require.NoError(t, encoder.Encode(&cstructs.StreamErrWrapper{Error: &cstructs.RpcError{Message: err.Error()}}))
			break
		}
	}

	decoder := codec.NewDecoder(p1, structs.MsgpackHandle)
	err = decoder.Decode(&resp)
	require.NoError(t, err)
	require.Empty(t, resp.ErrorMsg)

	t.Run("checking leader state", func(t *testing.T) {
		assertionFn(t, leader)
	})

	t.Run("checking nonleader state", func(t *testing.T) {
		assertionFn(t, leader)
	})
}

func TestOperator_SnapshotRestore_ACL(t *testing.T) {
	ci.Parallel(t)

	dir := t.TempDir()

	/////////  Actually run query now
	cases := []struct {
		name    string
		errCode int
		err     error
	}{
		{"root", 0, nil},
		{"no_permission_token", 403, structs.ErrPermissionDenied},
		{"invalid token", 403, structs.ErrPermissionDenied},
		{"unauthenticated", 403, structs.ErrPermissionDenied},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snapshot, _ := generateSnapshot(t)

			s, root, cleanupLS := TestACLServer(t, func(cfg *Config) {
				cfg.BootstrapExpect = 1
				cfg.DevMode = false
				cfg.DataDir = path.Join(dir, "server_"+c.name)
			})
			defer cleanupLS()

			testutil.WaitForLeader(t, s.RPC)

			deniedToken := mock.CreatePolicyAndToken(t, s.fsm.State(), 1001, "test-invalid", mock.NodePolicy(acl.PolicyWrite))

			token := ""
			switch c.name {
			case "root":
				token = root.SecretID
			case "no_permission_token":
				token = deniedToken.SecretID
			case "invalid token":
				token = uuid.Generate()
			case "unauthenticated":
				token = ""
			default:
				t.Fatalf("unexpected case: %v", c.name)
			}

			handler, err := s.StreamingRpcHandler("Operator.SnapshotRestore")
			require.NoError(t, err)

			p1, p2 := net.Pipe()
			defer p1.Close()
			defer p2.Close()

			// start handler
			go handler(p2)

			var req structs.SnapshotRestoreRequest
			var resp structs.SnapshotRestoreResponse

			req.Region = "global"
			req.AuthToken = token

			// send request
			encoder := codec.NewEncoder(p1, structs.MsgpackHandle)
			err = encoder.Encode(&req)
			require.NoError(t, err)

			if c.err == nil {
				buf := make([]byte, 1024)
				for {
					n, err := snapshot.Read(buf)
					if n > 0 {
						require.NoError(t, encoder.Encode(&cstructs.StreamErrWrapper{Payload: buf[:n]}))
					}
					if err != nil {
						require.NoError(t, encoder.Encode(&cstructs.StreamErrWrapper{Error: &cstructs.RpcError{Message: err.Error()}}))
						break
					}
				}
			}

			decoder := codec.NewDecoder(p1, structs.MsgpackHandle)
			err = decoder.Decode(&resp)
			require.NoError(t, err)

			// streaming errors appear as a response rather than a returned error
			if c.err != nil {
				require.Equal(t, c.err.Error(), resp.ErrorMsg)
				require.Equal(t, c.errCode, resp.ErrorCode)
				return

			}

			require.NotZero(t, resp.Index)

			io.Copy(io.Discard, p1)
		})
	}
}
