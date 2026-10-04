package session

import (
	"context"
	"sync"
	"time"

	"github.com/shhac/lib-agent-harness/completion"
)

// apiSession is the library's side of a session whose loop it runs.
type apiSession struct {
	store *transcript
	// workspace is the workbench's root, nil without one.
	workspace *workbenchHost
	// resultLimit bounds every answer to a call the model sees, as the host's
	// MaxResultBytes does for the answers it produces.
	resultLimit int
	complete    modelCall
	tools       []completion.Tool
	system      string
	recovery    Recovery

	mu        sync.Mutex
	records   []record
	responses int
	// cancelTurn stops the running turn's request and loop; loopDone closes
	// when that loop has returned.
	cancelTurn context.CancelFunc
	loopDone   chan struct{}
	stopping   bool
	// released closes once the transcript is closed and the lock given up.
	released chan struct{}
	// failed ends the session when a record cannot be made durable.
	failed func(error)
}

// append makes a record durable, then adds it to the history. A record that
// cannot be made durable ends the session.
func (a *apiSession) append(r record) error {
	r.At = time.Now().UTC()
	if err := a.store.append(r); err != nil {
		if a.failed != nil {
			a.failed(err)
		}
		return err
	}
	a.mu.Lock()
	a.records = append(a.records, r)
	a.mu.Unlock()
	return nil
}

func (a *apiSession) messages() []completion.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return conversation(a.records, a.system, a.resultLimit)
}

func (a *apiSession) nextResponse() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.responses++
	return a.responses
}

// begin registers a turn's loop, refusing once the session is stopping.
func (a *apiSession) begin(cancel context.CancelFunc) (chan struct{}, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopping {
		return nil, ErrClosed
	}
	done := make(chan struct{})
	a.cancelTurn, a.loopDone = cancel, done
	return done, nil
}

func (a *apiSession) interrupt() {
	a.mu.Lock()
	cancel := a.cancelTurn
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// shutdown stops the running turn and, once its loop has returned and every
// call the host admitted has settled, closes the transcript and the
// workspace, and gives up the lock. A handler still running keeps the conversation locked, so no other
// session can resume it and answer that call as unknown while it may still be
// taking effect; its result is recorded when it returns.
func (a *apiSession) shutdown(host *toolHost) {
	a.mu.Lock()
	if a.stopping {
		a.mu.Unlock()
		return
	}
	a.stopping = true
	cancel, loop := a.cancelTurn, a.loopDone
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	go func() {
		if loop != nil {
			<-loop
		}
		if host != nil {
			host.mu.Lock()
			settled := host.settled
			host.mu.Unlock()
			<-settled
		}
		if a.workspace != nil {
			_ = a.workspace.cleanupWrites(a.records)
			if a.workspace.commands != nil {
				if err := a.workspace.fromSandbox(a.workspace.commands.Close()); err != nil && a.workspace.failed != nil {
					a.workspace.failed(err)
				}
			}
		}
		a.store.close()
		a.workspace.close()
		close(a.released)
	}()
}
