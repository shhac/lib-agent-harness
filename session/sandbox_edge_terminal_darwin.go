package session

import (
	"bytes"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// foreignTerminal opens a pseudo-terminal outside any sandbox and names its
// terminal device, for the canary to prove the edge reaches only terminals
// it creates itself.
func foreignTerminal() (string, func(), error) {
	f, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return "", nil, err
	}
	fd := int(f.Fd())
	name := make([]byte, 128)
	for _, request := range []uint{unix.TIOCPTYGRANT, unix.TIOCPTYUNLK} {
		if err = unix.IoctlSetInt(fd, request, 0); err != nil {
			f.Close()
			return "", nil, err
		}
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&name[0]))); errno != 0 {
		f.Close()
		return "", nil, errno
	}
	path := string(name[:max(bytes.IndexByte(name, 0), 0)])
	if path == "" {
		f.Close()
		return "", nil, os.ErrNotExist
	}
	return path, func() { _ = f.Close() }, nil
}
