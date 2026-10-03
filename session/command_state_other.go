//go:build !darwin && !linux

package session

import "context"

func prepareCommandState(ctx context.Context, s *CommandSandbox, o *Options) error {
	return stateError(StateUnusable)
}
