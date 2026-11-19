package media

import "golang.org/x/sys/windows"

func volumeSpace(path string) (available, total uint64, err error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	err = windows.GetDiskFreeSpaceEx(name, &available, &total, nil)
	return
}
