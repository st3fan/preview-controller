//go:build !linux

package dashboard

func diskUsage(string) *capacity { return nil }
