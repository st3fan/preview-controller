package dashboard

import "syscall"

func diskUsage(workspace string) *capacity {
	var s syscall.Statfs_t
	if err := syscall.Statfs(workspace, &s); err != nil || s.Bsize <= 0 {
		return nil
	}
	size := uint64(s.Bsize)
	return usage(s.Blocks*size, s.Bfree*size, s.Bavail*size)
}
