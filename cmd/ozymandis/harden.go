package main

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// defaultTrustedProxies is who may speak for a client when nothing is
// configured: this machine and the private ranges.
//
// The ingress controller in front of the dashboard reaches it from one of
// these, so an ordinary install keeps seeing real client addresses without
// setting anything. What it stops is the case that mattered: a caller arriving
// directly from the internet has a public address, and its X-Forwarded-For is
// now its own claim about itself rather than a fact.
var defaultTrustedProxies = []string{
	"127.0.0.0/8", "::1/128",
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	"fc00::/7",
}

// parseTrustedProxies turns configured CIDRs (or bare addresses) into
// prefixes. An empty list means the defaults.
//
// A value that does not parse is an error rather than skipped. A typo here
// would otherwise silently stop trusting the real proxy, and every visitor
// would share its address — and its sign-in attempt budget.
func parseTrustedProxies(configured []string) ([]netip.Prefix, error) {
	if len(configured) == 0 {
		configured = defaultTrustedProxies
	}
	out := make([]netip.Prefix, 0, len(configured))
	for _, raw := range configured {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if p, err := netip.ParsePrefix(raw); err == nil {
			out = append(out, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			return nil, fmt.Errorf(
				"OZYMANDIS_TRUSTED_PROXIES: %q is neither an address nor a CIDR", raw)
		}
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

// realIP replaces RemoteAddr with the client's address, but only on the word
// of a proxy this install trusts.
//
// chi's RealIP did this unconditionally, so anybody could choose the address
// they were counted and recorded under by sending a header: the per-client
// sign-in limit was a limit on nobody, and the address stored on a session was
// whatever its holder said it was.
//
// The client is the rightmost X-Forwarded-For entry that is not itself a
// trusted proxy. Rightmost, because each proxy appends the peer it saw — the
// left of the list is whatever the original sender typed.
//
// With edgeSecret set, a trusted address is not enough: the request must also
// carry it in edgeHeader. That is what separates the ingress controller from a
// tenant's pod, which shares its address range and can reach this process just
// as directly. The header is removed either way, so nothing downstream — a
// log line, a handler — ever holds the secret.
func realIP(trusted []netip.Prefix, edgeSecret string) func(http.Handler) http.Handler {
	isTrusted := func(a netip.Addr) bool {
		a = a.Unmap()
		for _, p := range trusted {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fromEdge := edgeSecret == "" || subtle.ConstantTimeCompare(
				[]byte(r.Header.Get(edgeHeader)), []byte(edgeSecret)) == 1
			r.Header.Del(edgeHeader)

			peer, ok := peerAddr(r.RemoteAddr)
			if ok && fromEdge && isTrusted(peer) {
				if client, found := forwardedClient(r, isTrusted); found {
					r.RemoteAddr = client.String()
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// edgeHeader carries OZYMANDIS_EDGE_SECRET from the edge proxy.
const edgeHeader = "X-Ozymandis-Edge"

// forwardedClient reads the client address a trusted proxy reported.
func forwardedClient(r *http.Request, isTrusted func(netip.Addr) bool) (netip.Addr, bool) {
	// Every X-Forwarded-For header, not just the first: a sender can add a
	// header of its own ahead of the proxy's, and the proxy's is the last.
	var hops []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(h, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		addr, ok := parseHop(hops[i])
		if !ok {
			// A hop that is not an address ends the chain that can be believed.
			// What is left to go on is X-Real-IP, below.
			break
		}
		if !isTrusted(addr) {
			return addr.Unmap(), true
		}
	}

	if raw := strings.TrimSpace(r.Header.Get("X-Real-IP")); raw != "" {
		if addr, ok := parseHop(raw); ok {
			return addr.Unmap(), true
		}
	}
	return netip.Addr{}, false
}

// parseHop reads one forwarded address, with or without a port or brackets.
//
// Some load balancers write "203.0.113.9:4711" or "[2001:db8::1]". Refusing
// those would leave every visitor counted under the proxy's own address, so
// one person's failed sign-ins would lock everybody out.
func parseHop(raw string) (netip.Addr, bool) {
	raw = strings.TrimSpace(raw)
	if addr, err := netip.ParseAddr(raw); err == nil {
		return addr, true
	}
	if ap, err := netip.ParseAddrPort(raw); err == nil {
		return ap.Addr(), true
	}
	if addr, err := netip.ParseAddr(strings.Trim(raw, "[]")); err == nil {
		return addr, true
	}
	return netip.Addr{}, false
}

func peerAddr(remote string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr, true
}

// securityHeaders sets the response headers every page and API answer carries.
//
// frame-ancestors (and X-Frame-Options for browsers that predate it) is the
// one that closes a real hole: without it any site could put the dashboard in
// an invisible frame and have a signed-in person click "delete" on it. The
// same-origin check does not help there — the click is the person's own, made
// on the real page.
//
// The policy stops short of script-src on purpose. The pages carry inline
// scripts, and a script policy that is wrong breaks the dashboard for
// everybody; that is its own change, with nonces, rather than a line here.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy",
			"frame-ancestors 'none'; base-uri 'self'; form-action 'self'; object-src 'none'")
		next.ServeHTTP(w, r)
	})
}
