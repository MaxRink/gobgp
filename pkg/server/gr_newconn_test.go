// Copyright (C) 2026 Nippon Telegraph and Telephone Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	api "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/packet/bgp"
)

// rawPeer is a scripted BGP speaker on one TCP connection.
type rawPeer struct {
	t    *testing.T
	conn net.Conn
}

func dialRawPeer(t *testing.T, port int) *rawPeer {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return &rawPeer{t: t, conn: conn}
}

func (r *rawPeer) send(m *bgp.BGPMessage) {
	b, err := m.Serialize()
	require.NoError(r.t, err)
	_, err = r.conn.Write(b)
	require.NoError(r.t, err)
}

// recv returns the next message, or nil when the connection ends.
func (r *rawPeer) recv() *bgp.BGPMessage {
	_ = r.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	hdr := make([]byte, bgp.BGP_HEADER_LENGTH)
	if _, err := io.ReadFull(r.conn, hdr); err != nil {
		return nil
	}
	buf := make([]byte, binary.BigEndian.Uint16(hdr[16:18]))
	copy(buf, hdr)
	if _, err := io.ReadFull(r.conn, buf[bgp.BGP_HEADER_LENGTH:]); err != nil {
		return nil
	}
	m, err := bgp.ParseBGPMessage(buf)
	require.NoError(r.t, err)
	return m
}

// open exchanges OPEN and KEEPALIVE with graceful restart for IPv4
// unicast (forwarding state preserved).
func (r *rawPeer) open(restarting bool) {
	caps := []bgp.ParameterCapabilityInterface{
		bgp.NewCapMultiProtocol(bgp.RF_IPv4_UC),
		bgp.NewCapGracefulRestart(restarting, false, 120, []*bgp.CapGracefulRestartTuple{bgp.NewCapGracefulRestartTuple(bgp.RF_IPv4_UC, true)}),
	}
	r.send(bgp.NewBGPOpenMessage(2, 90, "2.2.2.2", []bgp.OptionParameterInterface{bgp.NewOptionParameterCapability(caps)}))
	r.send(bgp.NewBGPKeepAliveMessage())
	for m := r.recv(); m == nil || m.Header.Type != bgp.BGP_MSG_KEEPALIVE; m = r.recv() {
		require.NotNil(r.t, m, "session not up on this connection")
	}
}

// RFC 4724 section 4.2: a new connection from a peer that negotiated
// graceful restart, while its session is still Established, means that
// the peer restarted. The speaker ends the old session, keeps the peer's
// routes as stale, takes the new connection and removes the stale routes
// at End-of-RIB. Before, it closed the new connection and kept the old
// session until its hold timer expired.
func TestNewConnectionFromRestartingPeer(t *testing.T) {
	ctx := context.Background()
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())

	s := NewBgpServer()
	go s.Serve()
	require.NoError(t, s.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
		Asn: 1, RouterId: "1.1.1.1", ListenPort: int32(port), ListenAddresses: []string{"127.0.0.1"},
	}}))
	defer s.StopBgp(ctx, &api.StopBgpRequest{})
	require.NoError(t, s.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
		Conf:            &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 2},
		Transport:       &api.Transport{PassiveMode: true},
		GracefulRestart: &api.GracefulRestart{Enabled: true, HelperOnly: true, RestartTime: 120},
		AfiSafis: []*api.AfiSafi{{
			Config:            &api.AfiSafiConfig{Family: &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}, Enabled: true},
			MpGracefulRestart: &api.MpGracefulRestart{Config: &api.MpGracefulRestartConfig{Enabled: true}},
		}},
	}}))
	// The number of routes in the Adj-RIB-In. ListPeer only counts them;
	// ListPath would read the paths that the graceful restart marks stale.
	adjIn := func() int {
		n := 0
		_ = s.ListPeer(ctx, &api.ListPeerRequest{Address: "127.0.0.1"}, func(p *api.Peer) {
			for _, af := range p.GetAfiSafis() {
				n += int(af.GetState().GetReceived())
			}
		})
		return n
	}

	old := dialRawPeer(t, port)
	old.open(false)
	old.send(bgp.NewBGPUpdateMessage(nil, []bgp.PathAttributeInterface{
		bgp.NewPathAttributeOrigin(0), bgp.NewPathAttributeAsPath([]bgp.AsPathParamInterface{bgp.NewAs4PathParam(bgp.BGP_ASPATH_ATTR_TYPE_SEQ, []uint32{2})}),
		bgp.NewPathAttributeNextHop("127.0.0.1"),
	}, []*bgp.IPAddrPrefix{bgp.NewIPAddrPrefix(24, "198.51.100.0")}))
	require.Eventually(t, func() bool { return adjIn() == 1 }, 5*time.Second, 10*time.Millisecond)

	// The peer restarts and connects again before its old session ends.
	restarted := dialRawPeer(t, port)
	restarted.open(true)
	require.Equal(t, 1, adjIn(), "the routes stay as stale until End-of-RIB")
	restarted.send(bgp.NewEndOfRib(bgp.RF_IPv4_UC))
	require.Eventually(t, func() bool { return adjIn() == 0 }, 5*time.Second, 10*time.Millisecond,
		"stale routes not removed at End-of-RIB")
}
