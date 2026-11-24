//go:build !linux

package netproxy

import (
	"syscall"
	"time"
)

const deadPeerKeepAlive = 30 * time.Second

func deadPeerControl(_ string, _ string, _ syscall.RawConn) error { return nil }
