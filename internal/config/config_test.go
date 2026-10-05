package config

import (
	"encoding/base64"
	"testing"
)

func TestLoadLeavesTheGitHubProxyOffByDefault(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/x")
	t.Setenv("TOKEN_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("GH_PROXY_URL", "")
	t.Setenv("GH_PROXY_API_KEY", "")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.GhProxyUrl != defaults["GH_PROXY_URL"] || c.GhProxyApiKey != "" {
		t.Fatalf("the proxy must be off until it is configured: %q %q", c.GhProxyUrl, c.GhProxyApiKey)
	}

	t.Setenv("GH_PROXY_URL", "https://gh-proxy.example.test/ ")
	t.Setenv("GH_PROXY_API_KEY", " key1\n")
	if c, _ = Load(); c.GhProxyUrl != "https://gh-proxy.example.test/" || c.GhProxyApiKey != "key1" {
		t.Fatalf("got %q %q, want both trimmed", c.GhProxyUrl, c.GhProxyApiKey)
	}
}

func TestLoadTrimsInternalApiToken(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/x")
	t.Setenv("TOKEN_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("INTERNAL_API_TOKEN", " sekrit\n")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.InternalApiToken != "sekrit" {
		t.Fatalf("InternalApiToken = %q, want the trimmed secret", c.InternalApiToken)
	}
}
