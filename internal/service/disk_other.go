//go:build !unix

package service

// diskSpace is not measured on this system: no disk-space warning.
func diskSpace(string) (free, total int64, ok bool) { return 0, 0, false }
