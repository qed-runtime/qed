package agent

import "context"

type currentRunContextKey struct{}

// WithRunInfo returns a child context carrying host-authenticated Run identity
//
// Tool adapters use this when execution crosses a process or transport boundary
func WithRunInfo(ctx context.Context, info RunInfo) context.Context {
	return context.WithValue(ctx, currentRunContextKey{}, info)
}

// RunInfoFromContext returns the identity of the Run currently executing a Tool
func RunInfoFromContext(ctx context.Context) (RunInfo, bool) {
	info, ok := ctx.Value(currentRunContextKey{}).(RunInfo)
	return info, ok
}
