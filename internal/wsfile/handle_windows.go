package wsfile

import (
	"io/fs"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

const MountCheckSupported = true

const OpenFlags = os.O_RDONLY
const DirectoryFlags = OpenFlags

func MountID(f *os.File) (Mount, error) {
	var info windows.ByHandleFileInformation
	h := windows.Handle(f.Fd())
	if windows.GetFileInformationByHandle(h, &info) != nil {
		return Mount{}, ErrMountUnavailable
	}
	buf := make([]uint16, windows.MAX_PATH)
	var n uint32
	for {
		var err error
		n, err = windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil || n == 0 || n > 32768 {
			return Mount{}, ErrMountUnavailable
		}
		if n < uint32(len(buf)) {
			break
		}
		buf = make([]uint16, n+1)
	}
	return Mount{ID: uint64(info.VolumeSerialNumber), Path: strings.TrimRight(windows.UTF16ToString(buf[:n]), "\\")}, nil
}
func sameMount(a, b Mount) bool {
	return a.ID == b.ID && (strings.EqualFold(a.Path, b.Path) || strings.HasPrefix(strings.ToLower(a.Path), strings.ToLower(b.Path)+"\\"))
}
func handleFacts(f *os.File) (uint64, bool, error) {
	h := windows.Handle(f.Fd())
	kind, err := windows.GetFileType(h)
	if err != nil {
		return 0, false, err
	}
	if kind != windows.FILE_TYPE_DISK {
		return 0, false, nil
	}
	var info windows.ByHandleFileInformation
	err = windows.GetFileInformationByHandle(h, &info)
	return uint64(info.NumberOfLinks), kind == windows.FILE_TYPE_DISK, err
}
func LinkCount(fs.FileInfo) uint64 { return 0 }
func NotRegular(err error) bool    { return err == windows.ERROR_DIRECTORY }
