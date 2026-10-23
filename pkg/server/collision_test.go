package server

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

// scriptedPeer is the remote side of a connection collision: it plays the
// BGP messages of a peer by hand on two TCP connections.
type scriptedPeer struct {
	t   *testing.T
	asn uint32
	id  netip.Addr
}

func (p *scriptedPeer) write(c net.Conn, m *bgp.BGPMessage) {
	p.t.Helper()
	b, err := m.Serialize()
	require.NoError(p.t, err)
	_, err = c.Write(b)
	require.NoError(p.t, err)
}

func (p *scriptedPeer) open() *bgp.BGPMessage {
	p.t.Helper()
	m, err := bgp.NewBGPOpenMessage(uint16(p.asn), 90, p.id, []bgp.OptionParameterInterface{
		bgp.NewOptionParameterCapability([]bgp.ParameterCapabilityInterface{
			bgp.NewCapMultiProtocol(bgp.RF_IPv4_UC),
			bgp.NewCapFourOctetASNumber(p.asn),
		}),
	})
	require.NoError(p.t, err)
	return m
}

// read returns the next BGP message on c.
func (p *scriptedPeer) read(c net.Conn) *bgp.BGPMessage {
	p.t.Helper()
	require.NoError(p.t, c.SetReadDeadline(time.Now().Add(10*time.Second)))
	hdr := make([]byte, bgp.BGP_HEADER_LENGTH)
	_, err := io.ReadFull(c, hdr)
	require.NoError(p.t, err)
	body := make([]byte, int(binary.BigEndian.Uint16(hdr[16:18]))-bgp.BGP_HEADER_LENGTH)
	_, err = io.ReadFull(c, body)
	require.NoError(p.t, err)
	m, err := bgp.ParseBGPMessage(append(hdr, body...))
	require.NoError(p.t, err)
	return m
}

func (p *scriptedPeer) expect(c net.Conn, typ uint8) *bgp.BGPMessage {
	p.t.Helper()
	m := p.read(c)
	require.Equal(p.t, typ, m.Header.Type, "message %v", m.Body)
	return m
}

// A connection collision that GoBGP resolves itself (RFC 4271 Section 6.8):
// GoBGP connects at once to a scripted peer that also connects to GoBGP. The
// peer answers on GoBGP's connection only after GoBGP is in OpenConfirm or
// OpenSent on the peer's connection. GoBGP closes one connection with
// Cease/Connection Collision Resolution and must go on with the other one,
// as the peer does, not go to Idle for the idle hold time.
func TestCollisionResolvedByGoBGPKeepsSurvivingConnection(t *testing.T) {
	for _, tt := range []struct {
		name string
		// GoBGP has the higher BGP Identifier and keeps the connection it
		// opened.
		dominant bool
		// GoBGP is in OpenConfirm on the peer's connection (it got the
		// peer's OPEN there) when the OPEN on its own connection arrives;
		// otherwise it is still in OpenSent.
		openConfirm bool
		// frr: the peer sends OPEN and KEEPALIVE together on GoBGP's
		// connection and at once closes its own connection with Cease/
		// Connection Collision Resolution, as FRR does when it holds an
		// OPEN from GoBGP already.
		frr bool
	}{
		{"openconfirm, gobgp dominant", true, true, false},
		{"openconfirm, peer dominant", false, true, false},
		{"opensent, gobgp dominant", true, false, false},
		{"opensent, peer dominant", false, false, false},
		{"openconfirm, gobgp dominant, frr timing", true, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			lc := &net.ListenConfig{}
			peerL, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { peerL.Close() })
			l, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
			require.NoError(t, err)
			gobgpPort := l.Addr().(*net.TCPAddr).Port
			require.NoError(t, l.Close())

			gobgpID, peerID := "192.0.2.17", "10.0.0.1"
			if !tt.dominant {
				gobgpID, peerID = "10.0.0.1", "192.0.2.17"
			}
			s := NewBgpServer()
			go s.Serve()
			require.NoError(t, s.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
				Asn: 65001, RouterId: gobgpID, ListenPort: int32(gobgpPort), ListenAddresses: []string{"127.0.0.1"},
			}}))
			t.Cleanup(func() { _ = s.StopBgp(t.Context(), &api.StopBgpRequest{}) })
			p := &scriptedPeer{t: t, asn: 65002, id: netip.MustParseAddr(peerID)}

			w := newPeerStateWaiter(s, api.PeerState_SESSION_STATE_ESTABLISHED)
			start := time.Now()
			require.NoError(t, s.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
				Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65002},
				Transport: &api.Transport{RemotePort: uint32(peerL.Addr().(*net.TCPAddr).Port)},
				Timers:    &api.Timers{Config: &api.TimersConfig{ConnectAtOnce: true, IdleHoldTimeAfterReset: 1}},
			}}))

			// GoBGP's connection (A): its OPEN, the peer does not answer yet.
			a, err := peerL.Accept()
			require.NoError(t, err)
			t.Cleanup(func() { a.Close() })
			p.expect(a, bgp.BGP_MSG_OPEN)

			// The peer's connection (B).
			b, err := (&net.Dialer{}).DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(gobgpPort)))
			require.NoError(t, err)
			t.Cleanup(func() { b.Close() })
			if tt.openConfirm {
				p.write(b, p.open())
				p.expect(b, bgp.BGP_MSG_OPEN)
				p.expect(b, bgp.BGP_MSG_KEEPALIVE) // GoBGP is in OpenConfirm on B
				if tt.frr {
					oa, err := p.open().Serialize()
					require.NoError(t, err)
					ka, err := bgp.NewBGPKeepAliveMessage().Serialize()
					require.NoError(t, err)
					_, err = a.Write(append(oa, ka...))
					require.NoError(t, err)
					p.write(b, bgp.NewBGPNotificationMessage(bgp.BGP_ERROR_CEASE, bgp.BGP_ERROR_SUB_CONNECTION_COLLISION_RESOLUTION, nil))
					b.Close()
				} else {
					p.write(a, p.open())
				}
			} else {
				p.expect(b, bgp.BGP_MSG_OPEN) // GoBGP is in OpenSent on B
				p.write(a, p.open())
				time.Sleep(50 * time.Millisecond)
				p.write(b, p.open())
			}

			// The peer resolves the collision too and keeps the connection
			// that the speaker with the higher BGP Identifier opened.
			keep, drop := b, a
			if tt.dominant {
				keep, drop = a, b
			}
			if !tt.frr && (keep == a || !tt.openConfirm) {
				// GoBGP's KEEPALIVE on B in OpenConfirm was read above.
				p.expect(keep, bgp.BGP_MSG_KEEPALIVE)
			}
			if !tt.frr {
				p.write(keep, bgp.NewBGPKeepAliveMessage())
			}
			go func() {
				// GoBGP closes the other connection, with a NOTIFICATION
				// when it already sent its OPEN there.
				_, _ = io.Copy(io.Discard, drop)
			}()
			w.Wait(t, 10*time.Second)
			require.Less(t, time.Since(start), 2*time.Second, "GoBGP went to Idle instead of keeping the surviving connection")
		})
	}
}

// Like FRR after BFD comes up: the peer holds GoBGP's connection (A) without
// an answer, then at the same moment sends OPEN and KEEPALIVE on A and
// connects to GoBGP (B) with its OPEN. GoBGP has the higher BGP Identifier,
// so both sides keep A; the peer closes B with Cease/Connection Collision
// Resolution once it has GoBGP's OPEN there.
func TestCollisionSimultaneousOpenFRRTiming(t *testing.T) {
	for i := range 40 {
		ctx := t.Context()
		lc := &net.ListenConfig{}
		peerL, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
		require.NoError(t, err)
		l, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
		require.NoError(t, err)
		gobgpPort := l.Addr().(*net.TCPAddr).Port
		require.NoError(t, l.Close())

		s := NewBgpServer()
		go s.Serve()
		require.NoError(t, s.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
			Asn: 65001, RouterId: "192.0.2.17", ListenPort: int32(gobgpPort), ListenAddresses: []string{"127.0.0.1"},
		}}))
		p := &scriptedPeer{t: t, asn: 65002, id: netip.MustParseAddr("10.0.0.1")}
		w := newPeerStateWaiter(s, api.PeerState_SESSION_STATE_ESTABLISHED)
		require.NoError(t, s.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
			Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65002},
			Transport: &api.Transport{RemotePort: uint32(peerL.Addr().(*net.TCPAddr).Port)},
			Timers:    &api.Timers{Config: &api.TimersConfig{ConnectAtOnce: true, IdleHoldTimeAfterReset: 1}},
		}}))
		a, err := peerL.Accept()
		require.NoError(t, err)
		p.expect(a, bgp.BGP_MSG_OPEN)
		time.Sleep(20 * time.Millisecond)

		start := time.Now()
		oa, _ := p.open().Serialize()
		ka, _ := bgp.NewBGPKeepAliveMessage().Serialize()
		delay := time.Duration(i%8) * 250 * time.Microsecond
		go func() {
			time.Sleep(delay)
			_, _ = a.Write(append(oa, ka...))
		}()
		b, err := (&net.Dialer{}).DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(gobgpPort)))
		require.NoError(t, err)
		p.write(b, p.open())
		go func() {
			// The peer closes its own connection after GoBGP's OPEN on it.
			hdr := make([]byte, bgp.BGP_HEADER_LENGTH)
			if _, err := io.ReadFull(b, hdr); err == nil && hdr[18] == bgp.BGP_MSG_OPEN {
				n, _ := bgp.NewBGPNotificationMessage(bgp.BGP_ERROR_CEASE, bgp.BGP_ERROR_SUB_CONNECTION_COLLISION_RESOLUTION, nil).Serialize()
				_, _ = b.Write(n)
			}
			b.Close()
		}()
		go func() { _, _ = io.Copy(io.Discard, a) }()
		w.Wait(t, 15*time.Second)
		took := time.Since(start)
		_ = s.StopBgp(context.Background(), &api.StopBgpRequest{})
		a.Close()
		peerL.Close()
		require.Less(t, took, 2*time.Second, "iteration %d (delay %v): GoBGP went to Idle", i, delay)
	}
}

// When the connection that survived a collision fails too, both are gone:
// GoBGP starts again after IdleHoldTimeAfterReset (1 s here), not after the
// idle hold time that the NOTIFICATION backs off to (2 s).
func TestCollisionBothConnectionsDropped(t *testing.T) {
	ctx := t.Context()
	lc := &net.ListenConfig{}
	peerL, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { peerL.Close() })
	l, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	gobgpPort := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())

	s := NewBgpServer()
	go s.Serve()
	require.NoError(t, s.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
		Asn: 65001, RouterId: "192.0.2.17", ListenPort: int32(gobgpPort), ListenAddresses: []string{"127.0.0.1"},
	}}))
	t.Cleanup(func() { _ = s.StopBgp(context.Background(), &api.StopBgpRequest{}) })
	p := &scriptedPeer{t: t, asn: 65002, id: netip.MustParseAddr("10.0.0.1")}
	require.NoError(t, s.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
		Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65002},
		Transport: &api.Transport{RemotePort: uint32(peerL.Addr().(*net.TCPAddr).Port)},
		Timers:    &api.Timers{Config: &api.TimersConfig{ConnectAtOnce: true, IdleHoldTimeAfterReset: 1}},
	}}))

	a, err := peerL.Accept()
	require.NoError(t, err)
	defer a.Close()
	p.expect(a, bgp.BGP_MSG_OPEN)
	b, err := (&net.Dialer{}).DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(gobgpPort)))
	require.NoError(t, err)
	defer b.Close()
	p.write(b, p.open())
	p.expect(b, bgp.BGP_MSG_OPEN)
	p.expect(b, bgp.BGP_MSG_KEEPALIVE) // OpenConfirm on B
	p.write(a, p.open())
	p.expect(b, bgp.BGP_MSG_NOTIFICATION) // GoBGP keeps A
	p.expect(a, bgp.BGP_MSG_KEEPALIVE)
	// The peer drops A as well.
	p.write(a, bgp.NewBGPNotificationMessage(bgp.BGP_ERROR_CEASE, bgp.BGP_ERROR_SUB_CONNECTION_COLLISION_RESOLUTION, nil))
	dropped := time.Now()
	a.Close()

	again, err := peerL.Accept()
	require.NoError(t, err)
	defer again.Close()
	took := time.Since(dropped)
	require.Less(t, took, 1800*time.Millisecond, "GoBGP waited the backed-off idle hold time")
	require.Greater(t, took, 500*time.Millisecond, "GoBGP did not wait IdleHoldTimeAfterReset")
}

// fsmErrorThenEOF reports whether GoBGP answers the peer's NOTIFICATION only
// with FSM Error/Unexpected Message in OpenSent (RFC 4271 Section 8.2.2,
// Event 25) and closes the connection.
func fsmErrorThenEOF(t *testing.T, p *scriptedPeer, c net.Conn) bool {
	t.Helper()
	m := p.read(c)
	n, ok := m.Body.(*bgp.BGPNotification)
	if !ok || n.ErrorCode != bgp.BGP_ERROR_FSM_ERROR || n.ErrorSubcode != 1 {
		t.Logf("GoBGP sent %v", m.Body)
		return false
	}
	require.NoError(t, c.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err := c.Read(make([]byte, 1))
	return err != nil
}

// The peer sends a NOTIFICATION as its first message on a connection, for
// example a peer that drops the connection before it sends its OPEN (FRR
// sends its OPEN on connect, so its Cease/Connection Collision Resolution
// comes after an OPEN; this test covers peers that do not). GoBGP answers it
// with the FSM Error of RFC 4271 Section 8.2.2, closes that connection and
// goes on with the other one, without the 5 s idle hold.
func TestNotificationInsteadOfOpen(t *testing.T) {
	cease := bgp.NewBGPNotificationMessage(bgp.BGP_ERROR_CEASE, bgp.BGP_ERROR_SUB_CONNECTION_COLLISION_RESOLUTION, nil)
	for _, onIncoming := range []bool{true, false} {
		name := "outgoing connection"
		if onIncoming {
			name = "incoming connection"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			lc := &net.ListenConfig{}
			peerL, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { peerL.Close() })
			l, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
			require.NoError(t, err)
			gobgpPort := l.Addr().(*net.TCPAddr).Port
			require.NoError(t, l.Close())

			s := NewBgpServer()
			go s.Serve()
			require.NoError(t, s.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
				Asn: 65001, RouterId: "192.0.2.17", ListenPort: int32(gobgpPort), ListenAddresses: []string{"127.0.0.1"},
			}}))
			t.Cleanup(func() { _ = s.StopBgp(context.Background(), &api.StopBgpRequest{}) })
			p := &scriptedPeer{t: t, asn: 65002, id: netip.MustParseAddr("10.0.0.1")}
			w := newPeerStateWaiter(s, api.PeerState_SESSION_STATE_ESTABLISHED)
			require.NoError(t, s.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
				Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65002},
				Transport: &api.Transport{RemotePort: uint32(peerL.Addr().(*net.TCPAddr).Port)},
				Timers:    &api.Timers{Config: &api.TimersConfig{ConnectAtOnce: true, IdleHoldTimeAfterReset: 1}},
			}}))

			// GoBGP's connection (A).
			a, err := peerL.Accept()
			require.NoError(t, err)
			t.Cleanup(func() { a.Close() })
			p.expect(a, bgp.BGP_MSG_OPEN)
			start := time.Now()

			if !onIncoming {
				// (b) The peer drops GoBGP's connection with a
				// NOTIFICATION instead of its OPEN and opens its own.
				p.write(a, cease)
				require.True(t, fsmErrorThenEOF(t, p, a), "GoBGP's answer on its own connection")
				b, err := (&net.Dialer{}).DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(gobgpPort)))
				require.NoError(t, err)
				t.Cleanup(func() { b.Close() })
				p.write(b, p.open())
				p.expect(b, bgp.BGP_MSG_OPEN)
				p.expect(b, bgp.BGP_MSG_KEEPALIVE)
				p.write(b, bgp.NewBGPKeepAliveMessage())
			} else {
				// (a) The peer opens its own connection (B), gets
				// GoBGP's OPEN there and drops it with a NOTIFICATION
				// instead of its OPEN; it keeps GoBGP's connection.
				b, err := (&net.Dialer{}).DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(gobgpPort)))
				require.NoError(t, err)
				t.Cleanup(func() { b.Close() })
				p.expect(b, bgp.BGP_MSG_OPEN)
				p.write(b, cease)
				require.True(t, fsmErrorThenEOF(t, p, b), "GoBGP's answer on the peer's connection")
				time.Sleep(20 * time.Millisecond)
				p.write(a, p.open())
				p.expect(a, bgp.BGP_MSG_KEEPALIVE)
				p.write(a, bgp.NewBGPKeepAliveMessage())
			}
			w.Wait(t, 15*time.Second)
			require.Less(t, time.Since(start), 2*time.Second, "GoBGP waited the idle hold time")
		})
	}
}

// RFC 4271 Section 6.8 detects a collision when an OPEN arrives, so GoBGP
// must not close the peer's connection before the peer's OPEN arrived on it.
// Here GoBGP (dominant) gets the peer's OPEN on its own connection (A) first
// and is Established there, while the peer's connection (B) has no OPEN yet.
// FRR answers an early close of B by connecting again, and with graceful
// restart that new connection ends the session (BOOTy CI: two drops per
// fault). GoBGP keeps B open, answers the OPEN on B with Cease/Connection
// Collision Resolution, and the session on A stays: exactly one.
func TestCollisionLoserClosedOnlyAfterItsOpen(t *testing.T) {
	ctx := t.Context()
	lc := &net.ListenConfig{}
	peerL, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { peerL.Close() })
	l, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	gobgpPort := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())

	s := NewBgpServer()
	go s.Serve()
	require.NoError(t, s.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
		Asn: 65001, RouterId: "192.0.2.17", ListenPort: int32(gobgpPort), ListenAddresses: []string{"127.0.0.1"},
	}}))
	t.Cleanup(func() { _ = s.StopBgp(context.Background(), &api.StopBgpRequest{}) })
	p := &scriptedPeer{t: t, asn: 65002, id: netip.MustParseAddr("10.0.0.1")}
	require.NoError(t, s.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
		Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65002},
		Transport: &api.Transport{RemotePort: uint32(peerL.Addr().(*net.TCPAddr).Port)},
		Timers:    &api.Timers{Config: &api.TimersConfig{ConnectAtOnce: true, IdleHoldTimeAfterReset: 1}},
	}}))

	a, err := peerL.Accept()
	require.NoError(t, err)
	t.Cleanup(func() { a.Close() })
	p.expect(a, bgp.BGP_MSG_OPEN)
	b, err := (&net.Dialer{}).DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(gobgpPort)))
	require.NoError(t, err)
	t.Cleanup(func() { b.Close() })
	p.expect(b, bgp.BGP_MSG_OPEN) // GoBGP is in OpenSent on B

	w := newPeerStateWaiter(s, api.PeerState_SESSION_STATE_ESTABLISHED)
	oa, err := p.open().Serialize()
	require.NoError(t, err)
	ka, err := bgp.NewBGPKeepAliveMessage().Serialize()
	require.NoError(t, err)
	_, err = a.Write(append(oa, ka...))
	require.NoError(t, err)
	p.expect(a, bgp.BGP_MSG_KEEPALIVE)
	w.Wait(t, 5*time.Second)

	// B is still open: no message and no close before the peer's OPEN.
	require.NoError(t, b.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
	_, err = b.Read(make([]byte, 1))
	var ne net.Error
	require.ErrorAs(t, err, &ne, "GoBGP closed the peer's connection before its OPEN: %v", err)
	require.True(t, ne.Timeout())

	// The peer's OPEN on B: GoBGP closes B as the collision loser.
	p.write(b, p.open())
	m := p.expect(b, bgp.BGP_MSG_NOTIFICATION)
	n := m.Body.(*bgp.BGPNotification)
	require.Equal(t, uint8(bgp.BGP_ERROR_CEASE), n.ErrorCode)
	require.Equal(t, uint8(bgp.BGP_ERROR_SUB_CONNECTION_COLLISION_RESOLUTION), n.ErrorSubcode)

	// Exactly one session, which stays up.
	var peer *api.Peer
	require.Never(t, func() bool {
		_ = s.ListPeer(ctx, &api.ListPeerRequest{}, func(p *api.Peer) { peer = p })
		return peer.GetState().GetSessionState() != api.PeerState_SESSION_STATE_ESTABLISHED
	}, 500*time.Millisecond, 20*time.Millisecond)
	var established uint32
	require.NoError(t, s.mgmtOperation(func() error {
		established = s.neighborMap[netip.MustParseAddr("127.0.0.1")].fsm.pConf.ReadOnly().State.EstablishedCount
		return nil
	}, false))
	require.Equal(t, uint32(1), established, "sessions established")
}

// loserSetup brings GoBGP (dominant) to the state of
// TestCollisionLoserClosedOnlyAfterItsOpen up to the peer's OPEN: GoBGP's
// connection a, where GoBGP sent its OPEN, and the peer's connection b,
// where GoBGP sent its OPEN and waits in OpenSent. before is written on b
// before the collision.
func loserSetup(t *testing.T, before []byte) (*BgpServer, *scriptedPeer, net.Conn, net.Conn) {
	t.Helper()
	ctx := t.Context()
	lc := &net.ListenConfig{}
	peerL, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { peerL.Close() })
	l, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	gobgpPort := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	s := NewBgpServer()
	go s.Serve()
	require.NoError(t, s.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
		Asn: 65001, RouterId: "192.0.2.17", ListenPort: int32(gobgpPort), ListenAddresses: []string{"127.0.0.1"},
	}}))
	t.Cleanup(func() { _ = s.StopBgp(context.Background(), &api.StopBgpRequest{}) })
	p := &scriptedPeer{t: t, asn: 65002, id: netip.MustParseAddr("10.0.0.1")}
	require.NoError(t, s.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
		Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65002},
		Transport: &api.Transport{RemotePort: uint32(peerL.Addr().(*net.TCPAddr).Port)},
		Timers:    &api.Timers{Config: &api.TimersConfig{ConnectAtOnce: true, IdleHoldTimeAfterReset: 1}},
	}}))
	a, err := peerL.Accept()
	require.NoError(t, err)
	t.Cleanup(func() { a.Close() })
	p.expect(a, bgp.BGP_MSG_OPEN)
	b, err := (&net.Dialer{}).DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(gobgpPort)))
	require.NoError(t, err)
	t.Cleanup(func() { b.Close() })
	p.expect(b, bgp.BGP_MSG_OPEN)
	if len(before) > 0 {
		_, err = b.Write(before)
		require.NoError(t, err)
		time.Sleep(50 * time.Millisecond)
	}

	// The peer's OPEN and KEEPALIVE on a: Established there.
	w := newPeerStateWaiter(s, api.PeerState_SESSION_STATE_ESTABLISHED)
	oa, err := p.open().Serialize()
	require.NoError(t, err)
	ka, err := bgp.NewBGPKeepAliveMessage().Serialize()
	require.NoError(t, err)
	_, err = a.Write(append(oa, ka...))
	require.NoError(t, err)
	p.expect(a, bgp.BGP_MSG_KEEPALIVE)
	w.Wait(t, 5*time.Second)
	return s, p, a, b
}

// loserTrackers counts the goroutines that keep a losing connection.
func loserTrackers() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "trackCollisionLoser.func")
}

func sentNotifications(t *testing.T, s *BgpServer) uint64 {
	t.Helper()
	var n uint64
	require.NoError(t, s.ListPeer(t.Context(), &api.ListPeerRequest{}, func(p *api.Peer) {
		n = p.GetState().GetMessages().GetSent().GetNotification()
	}))
	return n
}

func requireNotification(t *testing.T, p *scriptedPeer, c net.Conn, code, subcode uint8) {
	t.Helper()
	n := p.expect(c, bgp.BGP_MSG_NOTIFICATION).Body.(*bgp.BGPNotification)
	require.Equal(t, []uint8{code, subcode}, []uint8{n.ErrorCode, n.ErrorSubcode})
}

// The losing connection answers what arrives on it as OpenSent does, without
// touching the counters of the session on the other connection.
func TestCollisionLoserAnswers(t *testing.T) {
	ob, err := (&scriptedPeer{t: t, asn: 65002, id: netip.MustParseAddr("10.0.0.1")}).open().Serialize()
	require.NoError(t, err)
	for _, tt := range []struct {
		name   string
		before []byte // on the peer's connection before the collision
		after  []byte // and after it
		// the NOTIFICATION that GoBGP sends, none for nil
		want []uint8
	}{
		// The OPEN header arrives before the collision and the body after
		// it: the message is not cut, it is an OPEN.
		{"split open", ob[:bgp.BGP_HEADER_LENGTH], ob[bgp.BGP_HEADER_LENGTH:], []uint8{bgp.BGP_ERROR_CEASE, bgp.BGP_ERROR_SUB_CONNECTION_COLLISION_RESOLUTION}},
		{"keepalive", nil, mustSerialize(t, bgp.NewBGPKeepAliveMessage()), []uint8{bgp.BGP_ERROR_FSM_ERROR, 1}},
		// RFC 4271 Section 8.2.2, OpenSent, Event 24: no answer.
		{"version error", nil, mustSerialize(t, bgp.NewBGPNotificationMessage(bgp.BGP_ERROR_OPEN_MESSAGE_ERROR, bgp.BGP_ERROR_SUB_UNSUPPORTED_VERSION_NUMBER, nil)), nil},
		{"cease", nil, mustSerialize(t, bgp.NewBGPNotificationMessage(bgp.BGP_ERROR_CEASE, bgp.BGP_ERROR_SUB_CONNECTION_COLLISION_RESOLUTION, nil)), []uint8{bgp.BGP_ERROR_FSM_ERROR, 1}},
		// RFC 4271 Section 6.1: a header without the all-ones marker.
		{"bad marker", nil, append(make([]byte, 16), 0, 19, bgp.BGP_MSG_KEEPALIVE), []uint8{bgp.BGP_ERROR_MESSAGE_HEADER_ERROR, bgp.BGP_ERROR_SUB_CONNECTION_NOT_SYNCHRONIZED}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, p, _, b := loserSetup(t, tt.before)
			sent := sentNotifications(t, s)
			_, err := b.Write(tt.after)
			require.NoError(t, err)
			if tt.want == nil {
				require.True(t, readsEOFOnly(t, b), "GoBGP answered")
			} else {
				requireNotification(t, p, b, tt.want[0], tt.want[1])
			}
			require.Eventually(t, func() bool { return loserTrackers() == 0 }, 2*time.Second, 10*time.Millisecond)
			require.Equal(t, sent, sentNotifications(t, s), "the session's NOTIFICATION counter changed")
		})
	}
}

// The goroutine that keeps the losing connection ends when the peer closes
// the connection and when the peer is deleted (the handler stops).
func TestCollisionLoserTrackerEnds(t *testing.T) {
	t.Run("peer closes", func(t *testing.T) {
		_, _, _, b := loserSetup(t, nil)
		require.Eventually(t, func() bool { return loserTrackers() == 1 }, 2*time.Second, 10*time.Millisecond)
		b.Close()
		require.Eventually(t, func() bool { return loserTrackers() == 0 }, 2*time.Second, 10*time.Millisecond)
	})
	t.Run("delete peer", func(t *testing.T) {
		s, _, _, b := loserSetup(t, nil)
		require.Eventually(t, func() bool { return loserTrackers() == 1 }, 2*time.Second, 10*time.Millisecond)
		require.NoError(t, s.DeletePeer(t.Context(), &api.DeletePeerRequest{Address: "127.0.0.1"}))
		require.True(t, readsEOFOnly(t, b), "the losing connection was not closed")
		require.Eventually(t, func() bool { return loserTrackers() == 0 }, 2*time.Second, 10*time.Millisecond)
	})
}

// RFC 4271 Section 8.2.2, OpenSent, Event 24: a NOTIFICATION with a version
// error gets no answer; other NOTIFICATIONs get the FSM Error.
func TestOpenSentVersionErrorGetsNoAnswer(t *testing.T) {
	ctx := t.Context()
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	gobgpPort := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	s := NewBgpServer()
	go s.Serve()
	require.NoError(t, s.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
		Asn: 65001, RouterId: "192.0.2.17", ListenPort: int32(gobgpPort), ListenAddresses: []string{"127.0.0.1"},
	}}))
	t.Cleanup(func() { _ = s.StopBgp(context.Background(), &api.StopBgpRequest{}) })
	require.NoError(t, s.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
		Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65002},
		Transport: &api.Transport{PassiveMode: true},
	}}))
	p := &scriptedPeer{t: t, asn: 65002, id: netip.MustParseAddr("10.0.0.1")}
	b, err := (&net.Dialer{}).DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(gobgpPort)))
	require.NoError(t, err)
	t.Cleanup(func() { b.Close() })
	p.expect(b, bgp.BGP_MSG_OPEN)
	p.write(b, bgp.NewBGPNotificationMessage(bgp.BGP_ERROR_OPEN_MESSAGE_ERROR, bgp.BGP_ERROR_SUB_UNSUPPORTED_VERSION_NUMBER, nil))
	require.True(t, readsEOFOnly(t, b), "GoBGP answered the version error")
}

func mustSerialize(t *testing.T, m *bgp.BGPMessage) []byte {
	t.Helper()
	b, err := m.Serialize()
	require.NoError(t, err)
	return b
}

// readsEOFOnly reports whether c is closed without a message from GoBGP.
func readsEOFOnly(t *testing.T, c net.Conn) bool {
	t.Helper()
	require.NoError(t, c.SetReadDeadline(time.Now().Add(5*time.Second)))
	b := make([]byte, 64)
	n, err := c.Read(b)
	if n > 0 {
		t.Logf("GoBGP sent %d bytes, type %d", n, b[min(n-1, 18)])
		return false
	}
	var ne net.Error
	return err != nil && (!errors.As(err, &ne) || !ne.Timeout())
}
