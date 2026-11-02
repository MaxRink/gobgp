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

// Package gr keeps the state of graceful restart. Helper keeps the restart
// of a peer: we are the Receiving Speaker of RFC 4724 and the Helper of
// RFC 9494. Restart keeps our own restart: we are the Restarting Speaker of
// RFC 4724. The package does no I/O: the caller passes events and runs the
// actions that come back, including the timers.
package gr

import (
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

// fBit is the F bit in the flags of an AFI/SAFI tuple of the Graceful
// Restart Capability (RFC 4724 Section 3) and of the Long-Lived Graceful
// Restart Capability (RFC 9494 Section 3): "The most significant bit".
const fBit = 0x80

// restartStateBit is the R bit in the Restart Flags of the Graceful Restart
// Capability (RFC 4724 Section 3): "The most significant bit is defined as
// the Restart State (R) bit, which can be used to avoid possible deadlock
// caused by waiting for the End-of-RIB marker when multiple BGP speakers
// peering with each other restart."
const restartStateBit = 0x08

// Negotiated is what a session negotiated with the peer. It is an argument:
// the caller passes it to Helper.SessionEstablished() and
// Helper.SessionDown().
type Negotiated struct {
	// Families are the families that the session negotiated.
	Families map[bgp.Family]bool
	// GR is the Graceful Restart Capability of the peer, or nil if
	// graceful restart is not negotiated: the peer did not send it, or we
	// did not advertise it. RFC 4724 Section 3: "If more than one instance
	// of the Graceful Restart Capability is carried in the capability
	// advertisement, the receiver of the advertisement MUST ignore all but
	// the last instance of the Graceful Restart Capability." GR is the
	// last instance.
	GR *bgp.CapGracefulRestart
	// LLGR is the Long-Lived Graceful Restart Capability of the peer, or
	// nil if long-lived graceful restart is not negotiated.
	LLGR *bgp.CapLongLivedGracefulRestart
}

// HasFamilies reports whether n has one or more families in its Graceful
// Restart Capability or its Long-Lived Graceful Restart Capability.
func (n *Negotiated) HasFamilies() bool {
	return len(n.grFamilies()) > 0 || len(n.llgrTuples()) > 0
}

// ForwardingStateFromOpen reports whether the OPEN of a peer that opens a new
// connection while the session is established says that the peer preserved
// forwarding state. Only such a peer is restarting; otherwise the new
// connection is a late collision and the established session must stay.
//
// The GR F bit wins per family. An LLGR tuple counts only for a family that
// the GR capability does not list, and only with a nonzero stale time: FRR
// helper-only peers always set the LLGR F bit but advertise stale time zero,
// which preserves no routes.
func ForwardingStateFromOpen(open *bgp.BGPMessage) bool {
	if open == nil || open.Header.Type != bgp.BGP_MSG_OPEN {
		return false
	}
	body, ok := open.Body.(*bgp.BGPOpen)
	if !ok {
		return false
	}
	grForward := make(map[bgp.Family]bool)
	llgr := make(map[bgp.Family]*bgp.CapLongLivedGracefulRestartTuple)
	for _, p := range body.OptParams {
		param, ok := p.(*bgp.OptionParameterCapability)
		if !ok {
			continue
		}
		for _, c := range param.Capability {
			switch c := c.(type) {
			case *bgp.CapGracefulRestart:
				// RFC 4724 Section 3: ignore all but the last instance.
				clear(grForward)
				for _, t := range c.Tuples {
					grForward[bgp.NewFamily(t.AFI, t.SAFI)] = t.Flags&fBit != 0
				}
			case *bgp.CapLongLivedGracefulRestart:
				clear(llgr)
				for _, t := range c.Tuples {
					llgr[bgp.NewFamily(t.AFI, t.SAFI)] = t
				}
			}
		}
	}
	for _, preserved := range grForward {
		if preserved {
			return true
		}
	}
	for f, t := range llgr {
		if _, ok := grForward[f]; ok {
			continue
		}
		if t.Flags&fBit != 0 && t.RestartTime > 0 {
			return true
		}
	}
	return false
}

// grFamilies returns the families that n negotiated and that are in its
// Graceful Restart Capability, with whether the F bit is set for each.
func (n *Negotiated) grFamilies() map[bgp.Family]bool {
	m := make(map[bgp.Family]bool)
	if n.GR == nil {
		return m
	}
	for _, t := range n.GR.Tuples {
		if f := bgp.NewFamily(t.AFI, t.SAFI); n.Families[f] {
			m[f] = t.Flags&fBit != 0
		}
	}
	return m
}

// llgrTuples returns the tuples of the Long-Lived Graceful Restart
// Capability of n for the families that n negotiated. It is empty without
// the Graceful Restart Capability too. RFC 9494 Section 4.2 speaks of "the
// LLGR and accompanying GR Capability".
func (n *Negotiated) llgrTuples() map[bgp.Family]*bgp.CapLongLivedGracefulRestartTuple {
	m := make(map[bgp.Family]*bgp.CapLongLivedGracefulRestartTuple)
	if n.GR == nil || n.LLGR == nil {
		return m
	}
	for _, t := range n.LLGR.Tuples {
		if f := bgp.NewFamily(t.AFI, t.SAFI); n.Families[f] {
			m[f] = t
		}
	}
	return m
}
