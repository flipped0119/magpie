package netproxy

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"testing"
)

func TestRequireProxyBlocksDirectInternetButAllowsLoopbackAndLAN(t *testing.T) {
	t.Setenv("MAGPIE_EGRESS_REQUIRE_PROXY", "1")

	extReq := &http.Request{URL: &url.URL{Scheme: "https", Host: "api.anthropic.com"}}
	if _, err := Func(extReq.WithContext(With(extReq.Context(), "direct"))); !errors.Is(err, ErrDirectEgressRefused) {
		t.Fatalf("expected ErrDirectEgressRefused for direct external request, got %v", err)
	}
	if p, err := Func(extReq.WithContext(With(extReq.Context(), "http://127.0.0.1:18080"))); err != nil || p == nil || p.Host != "127.0.0.1:18080" {
		t.Fatalf("expected explicit proxy to pass, got (%v, %v)", p, err)
	}

	loopReq := &http.Request{URL: &url.URL{Scheme: "http", Host: "127.0.0.1:3425"}}
	if p, err := Func(loopReq); err != nil || p != nil {
		t.Fatalf("loopback must stay direct even under MAGPIE_EGRESS_REQUIRE_PROXY, got (%v, %v)", p, err)
	}

	lanReq := &http.Request{URL: &url.URL{Scheme: "http", Host: "169.254.1.10:8080"}}
	if p, err := Func(lanReq.WithContext(With(lanReq.Context(), "direct"))); err != nil || p != nil {
		t.Fatalf("LAN/link-local address must remain allowed direct, got (%v, %v)", p, err)
	}
}

func TestEnvWithNormalizesSocks5ToSocks5hAndEnforcesDeadProxy(t *testing.T) {
	// Default preserves user's literal socks5:// string.
	plain := EnvWith("socks5://user:pass@127.0.0.1:1080", []string{"PATH=/usr/bin"})
	if !slices.Contains(plain, "HTTPS_PROXY=socks5://user:pass@127.0.0.1:1080") {
		t.Fatalf("default must preserve literal socks5://, got %v", plain)
	}

	// Under MAGPIE_EGRESS_REQUIRE_PROXY=1 (or MAGPIE_FORCE_SOCKS5H=1), socks5:// upgrades to socks5h:// and direct becomes DeadProxy.
	t.Setenv("MAGPIE_EGRESS_REQUIRE_PROXY", "1")
	got := EnvWith("socks5://user:pass@127.0.0.1:1080", []string{"PATH=/usr/bin"})
	if !slices.Contains(got, "HTTPS_PROXY=socks5h://user:pass@127.0.0.1:1080") {
		t.Fatalf("expected socks5:// upgraded to socks5h:// in exported env, got %v", got)
	}
	if !slices.Contains(got, "ALL_PROXY=socks5h://user:pass@127.0.0.1:1080") {
		t.Fatalf("expected ALL_PROXY upgraded to socks5h://, got %v", got)
	}

	directEnv := EnvWith("direct", []string{"PATH=/usr/bin"})
	if !slices.Contains(directEnv, "HTTPS_PROXY="+DeadProxy) {
		t.Fatalf("expected DeadProxy when MAGPIE_EGRESS_REQUIRE_PROXY=1 and choice=direct, got %v", directEnv)
	}
}
