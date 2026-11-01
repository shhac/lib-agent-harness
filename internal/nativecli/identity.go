package nativecli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// EdgeIdentity includes the executable behind a recognized npm Codex launcher.
// It streams hashes and probes only fixed installation paths, never caller trees.
func EdgeIdentity(binary string) (string, error) {
	return EdgeIdentityWithPath(binary, os.Getenv("PATH"))
}
func EdgeIdentityWithPath(binary, pathEnv string) (string, error) {
	installation, err := DiscoverEdgeInstallation(binary, pathEnv)
	if err != nil {
		return "", err
	}
	files := installation.Executables
	hash := sha256.New()
	for _, path := range files {
		f, e := os.Open(path)
		if e != nil {
			return "", e
		}
		st, e := f.Stat()
		if e != nil {
			f.Close()
			return "", e
		}
		fmt.Fprintf(hash, "%s\x00%d\x00%d\x00", path, st.Size(), st.ModTime().UnixNano())
		_, e = io.Copy(hash, f)
		f.Close()
		if e != nil {
			return "", e
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
