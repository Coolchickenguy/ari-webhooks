package ssrf

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

var ErrUnsafeAddress = errors.New("destination is a private or reserved address")

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
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", hostname)
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
	for _, addr := range addrs {
		if isBlockedAddr(addr) {
			return ResolveVerdict{Reason: hostname + " resolves to a private/internal address"}
		}
	}
	return ResolveVerdict{Safe: true}
}

// DialContext resolves addr, refuses any private or reserved IP, and dials only the vetted address. // authz: a public name that rebinds to an internal address must not get a connection
func DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if !hostIsSafe(host) {
		return nil, ErrUnsafeAddress
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	for _, ip := range addrs {
		if isBlockedAddr(ip) {
			continue
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
	}
	return nil, ErrUnsafeAddress
}

// Transport is http.DefaultTransport with every connection going through DialContext.
func Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil // a proxy would connect on our behalf and skip the per-address check
	t.DialContext = DialContext
	return t
}
