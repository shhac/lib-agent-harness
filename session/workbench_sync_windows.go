package session

import (
	"io/fs"
	"os"
)

// ACL inheritance controls confidentiality. NewFileMode must not set the
// Windows read-only attribute on an otherwise writable new file.
func workbenchFileMode(fs.FileMode) fs.FileMode { return 0600 }

func syncWorkbenchHandle(*os.File) error { return nil }

// Windows does not offer the Unix directory durability contract.
func syncWorkbenchDir(r *os.Root) error { return nil }
