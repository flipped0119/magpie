//go:build !linux

package lowmem

func hostMemoryLimitBytes() int64 { return 0 }
