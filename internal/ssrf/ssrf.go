package ssrf

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var (
	ipv6UniqueLocal = regexp.MustCompile(`^f[cd][0-9a-f]*:`)
	ipv6LinkLocal   = regexp.MustCompile(`^fe[89ab][0-9a-f]*:`)
	ipv4Mapped      = regexp.MustCompile(`(?i)^::ffff:`)
	nat64           = regexp.MustCompile(`(?i)^64:ff9b:`)
	dottedQuad      = regexp.MustCompile(`^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$`)
)

// IsSafeOutboundUrl ports ari's outbound.ts guard: true only for an http(s) URL
// whose host is a public name or address by literal. DNS is not resolved, so a
// name that rebinds to a private IP remains a documented residual risk.
func IsSafeOutboundUrl(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false // only clickable-web schemes may leave the building
	}
	host := strings.ToLower(u.Hostname()) // Hostname() already strips IPv6 brackets
	if host == "" {
		return false
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return false // loopback by name
	}
	if host == "0.0.0.0" || host == "::" || host == "::1" {
		return false // this-host and v6 loopback
	}
	if ipv6UniqueLocal.MatchString(host) || ipv6LinkLocal.MatchString(host) {
		return false // fc00::/7 and fe80::/10
	}
	if ipv4Mapped.MatchString(host) || nat64.MatchString(host) {
		return false // v4-mapped/NAT64 embeddings sidestep the dotted-quad checks below
	}
	if m := dottedQuad.FindStringSubmatch(host); m != nil {
		a, _ := strconv.Atoi(m[1])
		b, _ := strconv.Atoi(m[2])
		if a > 255 || b > 255 {
			return false // matches ari exactly: only the range-deciding octets are validated
		}
		if a == 0 || a == 127 || a == 10 {
			return false // this-host, loopback, RFC1918
		}
		if a == 169 && b == 254 {
			return false // link-local incl. cloud metadata
		}
		if a == 172 && b >= 16 && b <= 31 {
			return false // RFC1918
		}
		if a == 192 && b == 168 {
			return false // RFC1918
		}
	}
	return true
}

// OriginOf returns scheme+host for audit logs; webhook URLs embed tokens in path/query.
func OriginOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
