package faults

import (
	"context"
	"time"

	"github.com/monirz/cloudrig/core/clock"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// UnaryServerInterceptor applies the same rules to unary gRPC calls that the
// REST path applies to HTTP requests, so a single Set faults both protocols.
// The match is on the full method (/pkg.Service/Method), the latency runs on
// the injected clock, and the failure is a real gRPC status — the rule's
// canonical code, which gerr numbers the same as grpc/codes — not an HTTP
// status smuggled over gRPC. Streaming RPCs are not intercepted.
func (s *Set) UnaryServerInterceptor(clk clock.Clock) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		rule, ok := s.Match("", info.FullMethod)
		if !ok {
			return handler(ctx, req)
		}
		if rule.Latency > 0 && !sleep(ctx, clk, rule.Latency) {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		if !rule.FailsRequest() {
			return handler(ctx, req) // latency only: slow, then the real response
		}
		return nil, status.Error(codes.Code(rule.code()), rule.message())
	}
}

// sleep waits for d on the injected clock, reporting whether it elapsed rather
// than the call being cancelled first. Under a FakeClock it blocks until a test
// advances time, which is what makes an injected latency deterministic.
func sleep(ctx context.Context, clk clock.Clock, d time.Duration) bool {
	done := make(chan struct{})
	t := clk.AfterFunc(d, func() { close(done) })
	select {
	case <-done:
		return true
	case <-ctx.Done():
		t.Stop()
		return false
	}
}
