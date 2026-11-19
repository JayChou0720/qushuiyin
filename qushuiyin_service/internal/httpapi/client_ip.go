package httpapi

import (
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

func peerIP(r *http.Request) netip.Addr {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	return peer.Addr().WithZone("").Unmap()
}

func (s *Mock) clientIP(r *http.Request) netip.Addr {
	peer := peerIP(r)
	trusted := func(ip netip.Addr) bool {
		return slices.ContainsFunc(s.TrustedProxies, func(prefix netip.Prefix) bool { return prefix.Contains(ip) })
	}
	if !trusted(peer) {
		return peer
	}
	// A trusted proxy appends its immediate peer. Walk from that end, stopping
	// before any addresses supplied by the first untrusted hop.
	headers := r.Header.Values("X-Forwarded-For")
	for i := len(headers) - 1; i >= 0; i-- {
		chain := headers[i]
		for {
			comma := strings.LastIndexByte(chain, ',')
			addr, err := netip.ParseAddr(strings.TrimSpace(chain[comma+1:]))
			if err != nil || addr.Zone() != "" {
				return peer
			}
			addr = addr.Unmap()
			if !trusted(addr) || comma < 0 && i == 0 {
				return addr
			}
			if comma < 0 {
				break
			}
			chain = chain[:comma]
		}
	}
	if real := r.Header.Values("X-Real-IP"); len(real) == 1 {
		if addr, err := netip.ParseAddr(strings.TrimSpace(real[0])); err == nil && addr.Zone() == "" {
			return addr.Unmap()
		}
	}
	return peer
}
