package geoip

import (
	"net/http"
	"net/netip"
	"strings"
)

// ClientIP returns the address of the client that sent r.
//
// trustedHops is the number of reverse proxies in front of this process
// that append to X-Forwarded-For (load balancer, CDN, ...). With 0 the TCP
// peer address is used and the header is ignored. With N > 0 the chain
// "X-Forwarded-For entries..., TCP peer" is read from the right and the
// N-th entry before the peer is returned: that is the address seen by the
// outermost trusted proxy, and anything further left is client-supplied
// and not trusted. If the header has fewer entries than expected, the
// leftmost available one is used.
func ClientIP(r *http.Request, trustedHops int) netip.Addr {
	peer := parseAddr(r.RemoteAddr)
	if trustedHops <= 0 {
		return peer
	}
	// Walk the forwarded chain from the right without allocating.
	hops := trustedHops
	result := peer
	vals := r.Header.Values("X-Forwarded-For")
	for i := len(vals) - 1; i >= 0 && hops > 0; i-- {
		v := vals[i]
		for hops > 0 && v != "" {
			var part string
			if j := strings.LastIndexByte(v, ','); j >= 0 {
				part, v = v[j+1:], v[:j]
			} else {
				part, v = v, ""
			}
			a := parseAddr(strings.TrimSpace(part))
			if !a.IsValid() {
				return result // garbage in the chain: stop at the last good hop
			}
			result = a
			hops--
		}
	}
	return result
}

func parseAddr(s string) netip.Addr {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap()
	}
	if a, err := netip.ParseAddr(strings.Trim(s, "[]")); err == nil {
		return a.Unmap().WithZone("")
	}
	return netip.Addr{}
}
