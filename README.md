# GoBGP: BGP implementation in Go

[![Go Report Card](https://goreportcard.com/badge/github.com/osrg/gobgp)](https://goreportcard.com/report/github.com/osrg/gobgp)
[![Tests](https://github.com/osrg/gobgp/actions/workflows/ci.yml/badge.svg)](https://github.com/osrg/gobgp/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/osrg/gobgp/v4.svg)](https://pkg.go.dev/github.com/osrg/gobgp/v4)
[![Releases](https://img.shields.io/github/release/osrg/gobgp/all.svg?style=flat-square)](https://github.com/osrg/gobgp/releases)
[![LICENSE](https://img.shields.io/github/license/osrg/gobgp.svg?style=flat-square)](https://github.com/osrg/gobgp/blob/master/LICENSE)

GoBGP is an open source Border Gateway Protocol (BGP) implementation designed from scratch for
modern environment and implemented in a modern programming language,
[the Go Programming Language](http://golang.org/).

----

## Install

Try [a binary release](https://github.com/osrg/gobgp/releases/latest).

## Documentation

### Using GoBGP

- [Getting Started](docs/sources/getting-started.md)
- CLI
  - [Typical operation examples](docs/sources/cli-operations.md)
  - [Complete syntax](docs/sources/cli-command-syntax.md)
- [Route Server](docs/sources/route-server.md)
- [Route Reflector](docs/sources/route-reflector.md)
- [Policy](docs/sources/policy.md)
- Zebra Integration
  - [FIB manipulation](docs/sources/zebra.md)
  - [Equal Cost Multipath Routing](docs/sources/zebra-multipath.md)
- [MRT](docs/sources/mrt.md)
- [BMP](docs/sources/bmp.md)
- [EVPN](docs/sources/evpn.md)
- [Flowspec](docs/sources/flowspec.md)
- [RPKI](docs/sources/rpki.md)
- [Metrics](docs/sources/metrics.md)
- [Managing GoBGP with your favorite language with gRPC](docs/sources/grpc-client.md)
- Go Native BGP Library
  - [Basics](docs/sources/lib.md)
  - [BGP-LS](docs/sources/bgp-ls.md)
  - [SR Policy](docs/sources/lib-srpolicy.md)
- [Graceful Restart](docs/sources/graceful-restart.md)
- [Additional Paths](docs/sources/add-paths.md)
- [Peer Group](docs/sources/peer-group.md)
- [Dynamic Neighbor](docs/sources/dynamic-neighbor.md)
- [eBGP Multihop](docs/sources/ebgp-multihop.md)
- [TTL Security](docs/sources/ttl-security.md)
- [BFD](docs/sources/bfd.md)
- [Confederation](docs/sources/bgp-confederation.md)
- Data Center Networking
  - [Unnumbered BGP](docs/sources/unnumbered-bgp.md)
- [Sentry](docs/sources/sentry.md)

### Externals

- [Tutorial: Using GoBGP as an IXP connecting router](http://www.slideshare.net/shusugimoto1986/tutorial-using-gobgp-as-an-ixp-connecting-router)
- [GoBGP.nix: A NixOS module for GoBGP. Containing a working FRR implementation and a rich set of Options](https://github.com/wavelens/gobgp.nix)

## Community, discussion and support

We have the [Slack](https://join.slack.com/t/gobgp/shared_invite/zt-g9il5j8i-3gZwnXArK0O9Mnn4Yu~IrQ) for questions, discussion, suggestions, etc.

You have code or documentation for GoBGP? Awesome! Send a pull
request. No CLA, board members, governance, or other mess. See [`CONTRIBUTING.md`](CONTRIBUTING.md) for info on
code contributing.

## Licensing

GoBGP is licensed under the Apache License, Version 2.0. See
[LICENSE](https://github.com/osrg/gobgp/blob/master/LICENSE) for the full
license text.

## Fork branch booty/master

This branch is upstream GoBGP master at 6d26fa29 (2026-10-23, after
v4.10.0; there is no newer release) with the fixes and features that
BOOTy needs and upstream does not have yet, released as the tags
`v4.10.0-booty.N` from `v4.10.0-booty.23` on. `v4.10.0-booty.17` to
`v4.10.0-booty.22` were on upstream master 57fa92f1; the old commits stay
reachable through their tags. The idle hold time backs off like v_start
of FRR 10.2.1, not like upstream 4e70e768. The tags up to
`v4.10.0-booty.16` are on the branch `booty/v4.10` (based on v4.10.0). Do
not use `v4.10.0-booty.7`: it stops answering a NOTIFICATION in OpenSent,
which RFC 4271 Section 8.2.2 does not allow. A neighbor set entry can
carry an IPv6 zone ("fe80::1%eth0/128") that must match the zone of the
neighbor address.
