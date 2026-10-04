package completion

import (
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
)

// grokSessionDir finds the directory Grok keeps a launch's sessions in: one
// per working directory, named by the escaped path. A name that does not
// decode to work is matched by the cwd its session summary records instead.
func grokSessionDir(grokHome, work string) (string, bool) {
	sessions := filepath.Join(grokHome, "sessions")
	entries, err := os.ReadDir(sessions)
	if err != nil {
		return "", false
	}
	for _, entry := range entries {
		if name, err := url.PathUnescape(entry.Name()); err == nil && entry.IsDir() && sameWorkspace(name, work) {
			return filepath.Join(sessions, entry.Name()), true
		}
	}
	for _, entry := range entries {
		dir := filepath.Join(sessions, entry.Name())
		if session, ok := grokSingleSession(dir); ok && entry.IsDir() {
			var summary struct {
				Info struct {
					Cwd string `json:"cwd"`
				} `json:"info"`
			}
			raw, err := readBounded(filepath.Join(session, "summary.json"), 1<<20)
			if err == nil && json.Unmarshal(raw, &summary) == nil && summary.Info.Cwd != "" && sameWorkspace(summary.Info.Cwd, work) {
				return dir, true
			}
		}
	}
	return "", false
}

// grokSingleSession is the one session a fresh working directory holds.
func grokSingleSession(dir string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	found := ""
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if found != "" {
			return "", false
		}
		found = filepath.Join(dir, entry.Name())
	}
	return found, found != ""
}

// readGrokTranscript returns the persisted conversation of the one session
// launched in work, or nil when there is none to read.
func readGrokTranscript(grokHome, work string) []byte {
	dir, ok := grokSessionDir(grokHome, work)
	if !ok {
		return nil
	}
	session, ok := grokSingleSession(dir)
	if !ok {
		return nil
	}
	raw, err := readBounded(filepath.Join(session, "chat_history.jsonl"), grokTranscriptLimit)
	if err != nil {
		return nil
	}
	return raw
}

// removeGrokSession deletes a launch's persisted session, so prompts and
// replies do not accumulate in the runtime home.
func removeGrokSession(grokHome, work string) {
	if dir, ok := grokSessionDir(grokHome, work); ok {
		_ = os.RemoveAll(dir)
	}
}

func readBounded(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, os.ErrInvalid
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}
