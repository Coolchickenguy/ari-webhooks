package ssrf

import (
	"context"
	"errors"
	"testing"
)

var blockedUrls = []string{
	"not a url",
	"ftp://example.com/x",
	"javascript:alert(1)",
	"http://localhost/hook",
	"http://localhost./hook",
	"http://api.localhost/hook",
	"http://metadata.google.internal/computeMetadata/v1/",
	"http://files.corp/x",
	"http://printer.local/x",
	"http://wiki.intranet/x",
	"http://nas.lan/x",
	"http://router.home.arpa/x",
	"http://0.0.0.0/x",
	"http://[::]/x",
	"http://[::1]/x",
	"http://[0:0:0:0:0:0:0:1]/x",
	"http://[0000::1]/x",
	"http://[fc00::1]/x",
	"http://[fd12:3456::1]/x",
	"http://[fe80::1]/x",
	"http://[fe80::1%25eth0]/x",
	"http://[::ffff:7f00:1]/x",
	"http://[::ffff:127.0.0.1]/x",
	"http://[0:0:0:0:0:ffff:127.0.0.1]/x",
	"http://[0:0:0:0:0:ffff:169.254.169.254]/x",
	"http://[64:ff9b::7f00:1]/x",
	"http://[64:ff9b:1::7f00:1]/x",
	"http://[2002:7f00:1::1]/x",
	"http://[ff02::1]/x",
	"http://127.0.0.1/x",
	"http://127.9.9.9/x",
	"http://127.1/x",
	"http://2130706433/x",
	"http://0x7f000001/x",
	"http://10.0.0.5/x",
	"http://100.64.1.1/x",
	"http://169.254.169.254/latest/meta-data",
	"http://172.16.0.1/x",
	"http://172.31.255.255/x",
	"http://192.0.0.9/x",
	"http://192.168.1.1/x",
	"http://198.18.0.1/x",
	"http://224.0.0.1/x",
	"http://240.0.0.1/x",
	"http://0.1.2.3/x",
	"http://999.1.2.3/x",
}

var allowedUrls = []string{
	"https://hooks.example.com/x?token=abc",
	"https://example.com/x",
	"https://1password.example.com/x",
	"http://93.184.216.34/x",
	"https://172.15.0.1/x",
	"https://172.32.0.1/x",
	"https://193.168.1.1/x",
	"https://[2606:4700::1111]/x",
	"https://[2606:4700::6810:84e5]/x",
}

func TestIsSafeUrl(t *testing.T) {
	for _, u := range blockedUrls {
		if IsSafeUrl(u) {
			t.Errorf("IsSafeUrl(%s) must be blocked", u)
		}
		if IsSafeOutboundUrl(u) {
			t.Errorf("IsSafeOutboundUrl(%s) must be blocked", u)
		}
	}
	for _, u := range allowedUrls {
		if !IsSafeUrl(u) {
			t.Errorf("IsSafeUrl(%s) must be allowed", u)
		}
		if !IsSafeOutboundUrl(u) {
			t.Errorf("IsSafeOutboundUrl(%s) must be allowed", u)
		}
	}
}

func TestIsSafeIp(t *testing.T) {
	for _, ip := range []string{
		"127.0.0.1", "[127.0.0.1]", "0.0.0.0", "10.1.2.3", "100.64.1.1", "169.254.169.254", "172.16.0.1",
		"192.0.0.9", "192.168.1.1", "198.18.0.1", "224.0.0.1", "240.0.0.1",
		"::", "::1", "0:0:0:0:0:0:0:1", "0000::1", "[::1]", "::ffff:127.0.0.1", "0:0:0:0:0:ffff:127.0.0.1",
		"0:0:0:0:0:ffff:169.254.169.254", "::ffff:7f00:1", "64:ff9b::7f00:1", "64:ff9b:1::1", "2002:7f00:1::1",
		"fc00::1", "fd12::1", "fe80::1", "fe80::1%eth0", "ff02::1",
		"127.1", "2130706433", "0x7f000001", "", "not an ip",
	} {
		if IsSafeIp(ip) {
			t.Errorf("IsSafeIp(%q) must be false", ip)
		}
	}
	for _, ip := range []string{"93.184.216.34", "[93.184.216.34]", "172.15.0.1", "172.32.0.1", "2606:4700::1111", "[2606:4700::1111]"} {
		if !IsSafeIp(ip) {
			t.Errorf("IsSafeIp(%q) must be true", ip)
		}
	}
}

func TestDialContextRefusesInternalAddresses(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:80", "[::1]:80", "[::ffff:127.0.0.1]:80", "169.254.169.254:80", "localhost:80", "metadata.google.internal:80", "127.1:80"} {
		conn, err := DialContext(context.Background(), "tcp", addr)
		if conn != nil {
			conn.Close()
		}
		if !errors.Is(err, ErrUnsafeAddress) {
			t.Errorf("DialContext(%s) must refuse before resolving, got %v", addr, err)
		}
	}
	if _, err := DialContext(context.Background(), "tcp", "no-port"); err == nil {
		t.Error("an address without a port must be rejected")
	}
}

func TestOriginOf(t *testing.T) {
	if got := OriginOf("https://discord.com/api/webhooks/123/secrettoken?x=1"); got != "https://discord.com" {
		t.Fatalf("origin: %s", got)
	}
	if got := OriginOf("::nope"); got != "" {
		t.Fatalf("bad url origin should be empty, got %q", got)
	}
}
