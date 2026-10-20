package server

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/config/oc"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

// These tests pin GoBGP behaviour that BOOTy relies on when the carrier of
// an uplink comes back: it calls ResetPeer and then EnablePeer on a peer
// that carrier loss stopped (DisablePeer).

// stateEvents records the session states of the peer state events.
type stateEvents struct {
	mu     sync.Mutex
	states []api.PeerState_SessionState
}

func watchStateEvents(t *testing.T, s *BgpServer) *stateEvents {
	t.Helper()
	e := &stateEvents{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, s.WatchEvent(ctx, WatchEventMessageCallbacks{
		OnPeerUpdate: func(p *apiutil.WatchEventMessage_PeerEvent, _ time.Time) {
			if p == nil || p.Type != apiutil.PEER_EVENT_STATE {
				return
			}
			e.mu.Lock()
			e.states = append(e.states, api.PeerState_SessionState(int(p.Peer.State.SessionState)+1))
			e.mu.Unlock()
		},
	}, WatchPeer()))
	return e
}

func (e *stateEvents) reset() {
	e.mu.Lock()
	e.states = nil
	e.mu.Unlock()
}

func (e *stateEvents) get() []api.PeerState_SessionState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]api.PeerState_SessionState(nil), e.states...)
}

// ResetPeer and EnablePeer on a peer without a session send nothing: no
// NOTIFICATION on the old connection, and the next connection starts with
// the OPEN. The only peer state event is the FSM change from Idle to
// Active; ResetPeer and EnablePeer themselves fire none.
func TestResetAndEnableOfStoppedPeerSendNothing(t *testing.T) {
	const (
		asn      = 65001
		peerAddr = "10.0.0.1"
	)
	s, peer := startServerWithPassivePeer(t, asn, peerAddr, func(n *oc.Neighbor) {
		n.Timers.Config.IdleHoldTimeAfterReset = 1
	})
	ctx := context.Background()
	m1 := establishSession(t, s, peer, asn, peerAddr)
	events := watchStateEvents(t, s)

	w := newPeerStateWaiter(s, api.PeerState_SESSION_STATE_IDLE)
	require.NoError(t, s.DisablePeer(ctx, &api.DisablePeerRequest{Address: peerAddr}))
	w.Wait(t, 10*time.Second)
	time.Sleep(200 * time.Millisecond)
	sent := len(m1.GetSentMessages())
	events.reset()

	w = newPeerStateWaiter(s, api.PeerState_SESSION_STATE_ACTIVE)
	require.NoError(t, s.ResetPeer(ctx, &api.ResetPeerRequest{Address: peerAddr, BfdDown: true}))
	require.NoError(t, s.EnablePeer(ctx, &api.EnablePeerRequest{Address: peerAddr}))
	w.Wait(t, 10*time.Second)
	require.Eventually(t, func() bool { return len(events.get()) > 0 }, time.Second, 10*time.Millisecond)
	require.Equal(t, []api.PeerState_SessionState{api.PeerState_SESSION_STATE_ACTIVE}, events.get())
	require.Len(t, m1.GetSentMessages(), sent, "a message went out on the old connection")

	m2 := NewMockConnection()
	m2.SetRemoteAddr(peerAddr)
	t.Cleanup(func() { m2.Close() })
	peer.fsm.connCh <- m2
	require.Eventually(t, func() bool { return len(m2.GetSentMessages()) > 0 }, 5*time.Second, 10*time.Millisecond)
	first, err := bgp.ParseBGPMessage(m2.GetSentMessages()[0])
	require.NoError(t, err)
	require.Equal(t, uint8(bgp.BGP_MSG_OPEN), first.Header.Type)
}

// A peer that is administratively down in Idle keeps the stale routes of a
// graceful restart, and the restart timer keeps running in Idle: the routes
// go when it expires.
func TestAdminDownKeepsGracefulRestartRoutesUntilRestartTimer(t *testing.T) {
	const (
		asn      = 65001
		peerAddr = "10.0.0.1"
	)
	s, peer := startServerWithPassivePeer(t, asn, peerAddr, func(n *oc.Neighbor) {
		n.GracefulRestart.Config.Enabled = true
		n.GracefulRestart.Config.HelperOnly = true
		n.GracefulRestart.Config.RestartTime = 120
		n.AfiSafis[0].MpGracefulRestart.Config.Enabled = true
	})
	ctx := context.Background()
	adjIn := func() int {
		n := 0
		_ = s.ListPath(apiutil.ListPathRequest{TableType: api.TableType_TABLE_TYPE_ADJ_IN, Name: peerAddr, Family: bgp.RF_IPv4_UC},
			func(bgp.NLRI, []*apiutil.Path) { n++ })
		return n
	}

	m := NewMockConnection()
	m.SetRemoteAddr(peerAddr)
	w := newPeerStateWaiter(s, api.PeerState_SESSION_STATE_ESTABLISHED)
	peer.fsm.connCh <- m
	pushOpen(t, m, asn, peerAddr,
		bgp.NewCapMultiProtocol(bgp.RF_IPv4_UC),
		bgp.NewCapGracefulRestart(false, false, 3, []*bgp.CapGracefulRestartTuple{bgp.NewCapGracefulRestartTuple(bgp.RF_IPv4_UC, true)}))
	w.Wait(t, 10*time.Second)
	nh, err := bgp.NewPathAttributeNextHop(netip.MustParseAddr(peerAddr))
	require.NoError(t, err)
	nlri, err := bgp.NewIPAddrPrefix(netip.MustParsePrefix("198.51.100.0/24"))
	require.NoError(t, err)
	m.PushBgpMessage(bgp.NewBGPUpdateMessage(nil, []bgp.PathAttributeInterface{
		bgp.NewPathAttributeOrigin(0), bgp.NewPathAttributeAsPath(nil), nh, bgp.NewPathAttributeLocalPref(100),
	}, []bgp.PathNLRI{{NLRI: nlri}}))
	require.Eventually(t, func() bool { return adjIn() == 1 }, 5*time.Second, 10*time.Millisecond)

	// The TCP connection fails: graceful restart, the routes are stale.
	w = newPeerStateWaiter(s, api.PeerState_SESSION_STATE_IDLE)
	start := time.Now()
	m.Close()
	w.Wait(t, 10*time.Second)
	require.NoError(t, s.DisablePeer(ctx, &api.DisablePeerRequest{Address: peerAddr}))

	require.Never(t, func() bool { return adjIn() == 0 }, 1500*time.Millisecond, 50*time.Millisecond,
		"administrative shutdown dropped the stale routes")
	require.Eventually(t, func() bool { return adjIn() == 0 }, 10*time.Second, 50*time.Millisecond,
		"the restart timer did not expire in Idle")
	require.Greater(t, time.Since(start), 2500*time.Millisecond, "the stale routes went before the restart time (3 s)")
}
