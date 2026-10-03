// Package sandboxhook connects workspace fault injection to in-module tests.
// Configure hooks only before admitting work, or after it has settled.
package sandboxhook

import (
	"context"
	"os"
	"sync/atomic"
	"time"

	"github.com/shhac/lib-agent-harness/internal/wsfile"
)

type Hooks struct {
	Root           *os.Root
	WriteFault     *func(string) error
	Step, OpenStep *func()
	DirFault       *func(string, int) error
	Listed         *func(string)
	MaxVisited     *int
	Grace          *time.Duration
	Handles        *atomic.Int32
	Stopped        <-chan struct{}
	SetStuck       func(string)
}

var Access func(any) Hooks
var MountID *func(*os.File) (wsfile.Mount, error)

// RunnerHooks is available only to in-module tests. Configure before use.
type RunnerHooks struct {
	Close      *func() error
	SetExecute func(any)
}

var RunnerAccess func(any) RunnerHooks
var CanaryRuns func() uint64
var ResetCommandCache func()
var CommandCacheSize func() int
var CommandTrial func(context.Context, string, string, string) (string, string, error)

var NewCommandSandbox func(any, any, string, bool) any
var RecordCommandProof func(string)
var HasCommandProof func(string) bool
