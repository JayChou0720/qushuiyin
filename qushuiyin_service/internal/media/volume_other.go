//go:build !linux && !darwin && !windows

package media

import "errors"

func volumeSpace(string) (uint64, uint64, error) {
	return 0, 0, errors.New("media volume inspection unavailable on this operating system")
}
