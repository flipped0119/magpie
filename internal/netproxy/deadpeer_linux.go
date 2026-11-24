//go:build linux

package netproxy

import (
	"syscall"
	"time"
)

// tcpUserTimeoutMS is Linux's TCP_USER_TIMEOUT socket option (RFC 5482,
// syscall number 18 on Linux across amd64/arm64). Failing unacknowledged TCP
// sends after 60s prevents dead NAT/proxy peers from hanging HTTP/1.1 sockets
// (e.g. Codex) and holding memory/concurrency lanes for 15+ minutes.
const (
	linuxTCPUserTimeoutOpt = 18
	linuxTCPUserTimeoutMS  = 60_000
	deadPeerKeepAlive      = 30 * time.Second
)

func deadPeerControl(_ string, _ string, c syscall.RawConn) error {
	var sockErr error
	_ = c.Control(func(fd uintptr) {
		sockErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, linuxTCPUserTimeoutOpt, linuxTCPUserTimeoutMS)
	})
	// Best effort: ignore unsupported socket types (e.g. Unix/UDP).
	_ = sockErr
	return nil
}
