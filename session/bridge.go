package session

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// owner is what a live bridge writes into the lock it holds. Launch ties it to
// one specific launch record; a bridge from some other session, or a stale file
// left by one, will not match.
type owner struct {
	Group  int       `json:"group"`
	PID    int       `json:"pid"`
	Parent int       `json:"parent"`
	Launch string    `json:"launch"`
	HeldAt time.Time `json:"held_at"`
}

// RunBridge relays one harness's tool protocol to the session that configured
// it, and holds the lock that lets recovery identify the launch it belongs to.
// It is the whole implementation a caller's bridge command needs; it carries no
// policy, executes nothing and interprets nothing it relays.
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
	lock, err := holdBridgeLock(lockFile, filepath.Dir(socket))
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
	go func() {
		_, _ = io.Copy(conn, in)
		// Closing the write side lets the session observe the harness's end of
		// the stream instead of waiting on a half-open connection.
		if half, ok := conn.(*net.UnixConn); ok {
			_ = half.CloseWrite()
		}
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(out, conn)
	}()
	// The harness's end of the stream is the authority on when relaying is over.
	// A cancelled context ends it too, without waiting on a read from a harness
	// that may never write again.
	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}
