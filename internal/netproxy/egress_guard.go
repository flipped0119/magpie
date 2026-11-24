package netproxy

import (
	"errors"
	"os"
	"strings"
)

// ErrDirectEgressRefused is returned when MAGPIE_EGRESS_REQUIRE_PROXY is
// enabled and a non-loopback, non-LAN request resolves to no proxy (direct).
var ErrDirectEgressRefused = errors.New("netproxy: direct egress refused because MAGPIE_EGRESS_REQUIRE_PROXY is enabled")

// RequireProxy reports whether direct internet egress is forbidden process-wide.
// Enabled by MAGPIE_EGRESS_REQUIRE_PROXY=1|true|yes|on.
func RequireProxy() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MAGPIE_EGRESS_REQUIRE_PROXY"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// ForceSocks5h reports whether socks5:// should be upgraded to socks5h:// when
// exporting *_PROXY env vars to child CLIs. Automatically enabled under
// MAGPIE_EGRESS_REQUIRE_PROXY=1 or explicitly via MAGPIE_FORCE_SOCKS5H=1.
func ForceSocks5h() bool {
	if RequireProxy() {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MAGPIE_FORCE_SOCKS5H"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// normalizeExportProxy upgrades socks5:// to socks5h:// when ForceSocks5h() is
// active so child CLIs (curl/Node/Bun/Python/claude/agy) resolve hostnames on
// the proxy side and never leak local DNS.
func normalizeExportProxy(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if !ForceSocks5h() {
		return trimmed
	}
	if len(trimmed) >= len("socks5://") && strings.EqualFold(trimmed[:len("socks5://")], "socks5://") {
		return "socks5h://" + trimmed[len("socks5://"):]
	}
	return trimmed
}
