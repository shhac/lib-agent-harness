package session

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

// owner records who holds a session's bridge lock. It is written by the live
// bridge rather than predicted before launch, so recovery acts on what is
// actually running rather than on what was once intended.
type owner struct {
	Group  int       `json:"group"`
	PID    int       `json:"pid"`
	Parent int       `json:"parent"`
	HeldAt time.Time `json:"held_at"`
}

// Reclamation describes what a recovery attempt established.
type Reclamation struct {
	// Found reports that a surviving harness subtree was observed.
	Found bool
	// Reclaimed reports that it is now gone, confirmed by the lock being free.
	Reclaimed bool
	// Group is the process group that was terminated, when one was recorded.
	Group int
}

// ErrUnreclaimed reports a harness subtree that was observed and could not be
// confirmed gone. Treat the work as still reserved: starting another worker for
// the same assignment would leave two of them spending the same account.
var ErrUnreclaimed = errors.New("harness subtree could not be confirmed terminated")

// Reclaim ends a harness subtree orphaned by a crash of the process that
// launched it. Stopping whatever a session was operating on does not establish
// that the harness itself stopped: it keeps its provider connection and keeps
// spending. dir is the tool host's private directory from the interrupted run.
//
// A free lock means nothing survived. A held lock names the group that holds
// it; that group is terminated and the lock re-checked, and only a lock that
// has become free is reported as reclaimed.
func Reclaim(ctx context.Context, dir string) (Reclamation, error) {
	var out Reclamation
	if !restrictedPlatform() {
		return out, &CapabilityError{Code: CapabilityUnsupportedPlatform}
	}
	path := lockPath(dir)
	held, err := readBridgeLock(path)
	if err != nil {
		return out, err
	}
	if held == nil {
		return out, nil
	}
	out.Found, out.Group = true, held.Group
	if held.Group <= 1 {
		return out, ErrUnreclaimed
	}
	if err = terminateGroup(held.Group); err != nil {
		return out, errors.Join(ErrUnreclaimed, err)
	}
	// A signalled group takes a moment to be reaped. Re-read rather than assume.
	for attempt := 0; attempt < 50; attempt++ {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		remaining, readErr := readBridgeLock(path)
		if readErr != nil {
			return out, errors.Join(ErrUnreclaimed, readErr)
		}
		if remaining == nil {
			out.Reclaimed = true
			return out, nil
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return out, ErrUnreclaimed
}

func lockPath(dir string) string { return dir + string(os.PathSeparator) + "bridge.lock" }

// RunBridge relays one harness's tool protocol to the session that configured
// it, and holds the lock that makes an orphaned subtree discoverable. It is the
// whole implementation a caller's bridge command needs; it carries no policy,
// executes nothing and interprets nothing it relays.
//
// The channel, its credential and the lock are named by the environment
// variables the session set for this process. The credential is read from an
// owner-only file and sent once; it never appears in arguments, in relayed
// content, or in anything a model can ask for.
func RunBridge(ctx context.Context, in io.Reader, out io.Writer) error {
	socket := os.Getenv(BridgeSocketEnv)
	secretFile := os.Getenv(BridgeSecretEnv)
	lockFile := os.Getenv(BridgeLockEnv)
	if socket == "" || secretFile == "" || lockFile == "" {
		return errors.New("bridge must be started by a harness session")
	}
	lock, err := holdBridgeLock(lockFile)
	if err != nil {
		return err
	}
	defer lock.Close()
	secret, err := os.ReadFile(secretFile)
	if err != nil {
		return errors.New("bridge credential is unavailable")
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return errors.New("bridge could not reach its session")
	}
	defer conn.Close()
	hello, err := json.Marshal(map[string]string{"secret": strings.TrimSpace(string(secret))})
	if err != nil {
		return errors.New("bridge could not present its credential")
	}
	if _, err = conn.Write(append(hello, '\n')); err != nil {
		return errors.New("bridge could not present its credential")
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(out, conn)
	}()
	_, _ = io.Copy(conn, in)
	// Closing the write side lets the session observe the harness's end of the
	// stream instead of waiting on a half-open connection.
	if half, ok := conn.(*net.UnixConn); ok {
		_ = half.CloseWrite()
	}
	<-done
	return nil
}
