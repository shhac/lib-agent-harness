package wsfile

import (
	"errors"
	"os"
)

// ErrMountUnavailable means this runtime cannot establish mount identity.
var ErrMountUnavailable = errors.New("workbench mount check unavailable")

// Mount identifies an opened root's mount and Windows final path.
type Mount struct {
	ID   uint64
	Path string
}

// Handle contains facts established on the opened handle.
type Handle struct {
	Regular, Directory, SameMount bool
	Links                         uint64
}

func Check(f *os.File, root Mount) (Handle, error) {
	info, err := f.Stat()
	if err != nil {
		return Handle{}, err
	}
	links, disk, err := handleFacts(f)
	if err != nil {
		return Handle{}, err
	}
	if !disk {
		return Handle{}, nil
	}
	mount, err := MountID(f)
	if err != nil {
		return Handle{}, err
	}
	return Handle{Regular: disk && info.Mode().IsRegular(), Directory: disk && info.IsDir(), Links: links, SameMount: sameMount(mount, root)}, nil
}
