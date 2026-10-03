// Package sandboxbridge connects session orchestration to sandbox-owned internal
// mechanisms without adding public integration helpers. Sandbox registers the
// functions at initialization; users of this bridge must also import sandbox.
// Options and Proof cross this internal boundary opaquely to avoid an import cycle.
package sandboxbridge

import (
	"context"
	"io/fs"
	"os"
)

var NormalizeWorkbench func(any) (any, error)
var ProveWorkbench func(context.Context, any) (any, error)
var PrivateStateDir func(fs.FileInfo) bool
var LockState func(string) (*os.File, error)
