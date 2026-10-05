package ssrf

import "testing"

func TestIsSafeOutboundUrl(t *testing.T) {
	blocked := []string{
		"not a url",
		"ftp://example.com/x",
		"javascript:alert(1)",
		"http://localhost/hook",
		"http://api.localhost/hook",
		"http://0.0.0.0/x",
		"http://[::]/x",
		"http://[::1]/x",
		"http://[fc00::1]/x",
		"http://[fd12:3456::1]/x",
		"http://[fe80::1]/x",
		"http://[::ffff:7f00:1]/x",
		"http://[::ffff:127.0.0.1]/x",
		"http://[64:ff9b::7f00:1]/x",
		"http://127.0.0.1/x",
		"http://127.9.9.9/x",
		"http://10.0.0.5/x",
		"http://169.254.169.254/latest/meta-data",
		"http://172.16.0.1/x",
		"http://172.31.255.255/x",
		"http://192.168.1.1/x",
		"http://0.1.2.3/x",
		"http://999.1.2.3/x",
	}
	for _, u := range blocked {
		if IsSafeOutboundUrl(u) {
			t.Errorf("%s must be blocked", u)
		}
	}
	allowed := []string{
		"https://hooks.example.com/x?token=abc",
		"http://93.184.216.34/x",
		"https://172.15.0.1/x",
		"https://172.32.0.1/x",
		"https://193.168.1.1/x",
		"https://[2606:4700::6810:84e5]/x",
	}
	for _, u := range allowed {
		if !IsSafeOutboundUrl(u) {
			t.Errorf("%s must be allowed", u)
		}
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
