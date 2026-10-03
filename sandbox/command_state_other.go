//go:build !darwin && !linux

package sandbox

import "context"

func prepareCommandState(ctx context.Context, s *Sandbox, o *Options) error {
	return stateError(StateUnusable)
}
