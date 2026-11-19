//go:build linux || darwin

package media

import "syscall"

func volumeSpace(path string) (available, total uint64, err error) {
	var fs syscall.Statfs_t
	if err = syscall.Statfs(path, &fs); err != nil {
		return 0, 0, err
	}
	return uint64(fs.Bavail) * uint64(fs.Bsize), uint64(fs.Blocks) * uint64(fs.Bsize), nil
}
