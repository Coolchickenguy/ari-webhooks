package ssrf

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"
)

// IsSafeIp ports ari's ssrf.ts isSafeIp: false when the address (v4 or v6,
// with or without brackets) is private, loopback, link-local, or reserved.
func IsSafeIp(addr string) bool {
	ip := strings.Trim(strings.ToLower(addr), "[]")

	if m := dottedQuad.FindStringSubmatch(ip); m != nil {
		a := atoiByte(m[1])
		b := atoiByte(m[2])
		switch {
		case a == 127, a == 10: // loopback, private
			return false
		case a == 172 && b >= 16 && b <= 31: // private
			return false
		case a == 192 && b == 168: // private
			return false
		case a == 169 && b == 254: // link-local / metadata
			return false
		case a == 100 && b >= 64 && b <= 127: // shared space
			return false
		case a == 0, a >= 224: // this-host, multicast / reserved
			return false
		}
		return true
	}

	if ip == "::1" || ip == "::" {
		return false // v6 loopback and unspecified
	}
	if strings.HasPrefix(ip, "::ffff:") || strings.HasPrefix(ip, "64:ff9b:") {
		return false // v4-mapped/NAT64 reach v4 internals through the v6 literal
	}
	first := parseHex16(strings.SplitN(ip, ":", 2)[0])
	if first&0xfe00 == 0xfc00 || first&0xffc0 == 0xfe80 || first&0xff00 == 0xff00 {
		return false // ULA, link-local, multicast
	}
	return true
}

// IsSafeUrl ports ssrf.ts isSafeUrl: literal-only checks; names still need ResolvesSafely.
func IsSafeUrl(rawUrl string) bool {
	u, err := url.Parse(rawUrl)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false // only web schemes may be fetched
	}
	host := strings.ToLower(u.Hostname()) // brackets already stripped
	if host == "" {
		return false
	}
	if host == "localhost" || host == "0.0.0.0" {
		return false // loopback and this-host by name
	}
	for _, tld := range []string{".local", ".internal", ".intranet", ".corp", ".lan"} { // suffixes that name internal networks, never public hosts
		if strings.HasSuffix(host, tld) {
			return false
		}
	}
	if strings.Contains(host, ":") { // IPv6 literal
		return IsSafeIp(host)
	}
	if host[0] >= '0' && host[0] <= '9' {
		return IsSafeIp(host) // leading digit means IPv4 literal, range-check it here
	}
	return true
}

type ResolveVerdict struct {
	Safe      bool
	Transient bool
	Reason    string
}

// ResolvesSafely resolves the hostname and verifies every address is public.
// Transient DNS failures are retryable; a resolved-but-private host is not.
func ResolvesSafely(ctx context.Context, hostname string) ResolveVerdict {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIP(ctx, "ip", hostname)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return ResolveVerdict{Reason: hostname + " does not exist (ENOTFOUND)"}
		}
		return ResolveVerdict{Transient: true, Reason: "dns lookup failed (" + err.Error() + ")"}
	}
	if len(addrs) == 0 {
		return ResolveVerdict{Reason: hostname + " resolved to no addresses"}
	}
	for _, a := range addrs {
		if !IsSafeIp(a.String()) {
			return ResolveVerdict{Reason: hostname + " resolves to a private/internal address"}
		}
	}
	return ResolveVerdict{Safe: true}
}

func atoiByte(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func parseHex16(s string) int {
	if s == "" {
		return 0 // parseInt('', 16) is NaN in TS, and NaN in JS bitwise ops reads as 0
	}
	n := 0
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			n = n*16 + int(c-'0')
		case c >= 'a' && c <= 'f':
			n = n*16 + int(c-'a'+10)
		default:
			return 0
		}
	}
	return n
}
