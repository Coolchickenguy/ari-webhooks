package ssrf

import (
	"net/netip"
	"net/url"
	"strings"
)

// Ranges that are never a public destination, on top of what netip classifies
// itself: this-host, shared address space, IETF protocol assignments, benchmarking,
// class E, NAT64 and 6to4 (both embed a v4 address a resolver would never show).
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, cidr := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "64:ff9b::/96", "64:ff9b:1::/48", "fc00::/7", "fe80::/10", "ff00::/8", "2002::/16",
	} {
		out = append(out, netip.MustParsePrefix(cidr))
	}
	return out
}()

func parseAddr(host string) (netip.Addr, bool) {
	host = strings.Trim(host, "[]")
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i] // a zone names an interface, never a different destination
	}
	addr, err := netip.ParseAddr(host)
	return addr, err == nil
}

func isBlockedAddr(addr netip.Addr) bool {
	addr = addr.Unmap() // ::ffff:127.0.0.1 and 0:0:0:0:0:ffff:7f00:1 are 127.0.0.1 on the wire
	if addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return true
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func looksNumeric(host string) bool {
	if !strings.Contains(host, ":") && (host[0] < '0' || host[0] > '9') {
		return false
	}
	for _, c := range host {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && c != 'x' && c != '.' && c != ':' {
			return false // a letter outside hex spelling means a real hostname such as 1password.com
		}
	}
	return true
}

// hostIsSafe is the one check every guard shares: a host (name or address
// literal, with or without brackets) that must not be reached from here.
func hostIsSafe(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".") // DNS treats localhost. and localhost as the same name
	if host == "" {
		return false
	}
	if addr, ok := parseAddr(host); ok {
		return !isBlockedAddr(addr)
	}
	if looksNumeric(host) {
		return false // 127.1, 2130706433 and 0x7f000001 are address shorthands the resolver can still turn into loopback
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return false // loopback by name
	}
	for _, suffix := range []string{".local", ".internal", ".intranet", ".corp", ".lan", ".home.arpa"} { // suffixes that name internal networks, never public hosts
		if strings.HasSuffix(host, suffix) {
			return false
		}
	}
	return true
}

// IsSafeIp is false when the address (v4 or v6, with or without brackets) is
// private, loopback, link-local, reserved, or not an address at all.
func IsSafeIp(addr string) bool {
	parsed, ok := parseAddr(strings.TrimSpace(addr))
	return ok && !isBlockedAddr(parsed)
}

// IsSafeUrl is the literal-only check: true for an http(s) URL whose host is a
// public name or address. A name still needs ResolvesSafely or DialContext.
func IsSafeUrl(rawUrl string) bool {
	u, err := url.Parse(rawUrl)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false // only web schemes may be fetched
	}
	return hostIsSafe(u.Hostname())
}

// IsSafeOutboundUrl guards program webhook destinations with the same rules.
func IsSafeOutboundUrl(raw string) bool {
	return IsSafeUrl(raw)
}

// OriginOf returns scheme+host for audit logs; webhook URLs embed tokens in path/query.
func OriginOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
