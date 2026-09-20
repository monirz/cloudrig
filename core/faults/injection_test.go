package faults

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/monirz/cloudrig/core/clock"
)

var faultEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// TestRateFailsADeterministicFraction holds that a rate fails an exact fraction
// in a fixed pattern, so a fault composes with time travel into a reproducible
// test rather than a coin flip: 0.2 fails the 5th and 10th of ten requests.
func TestRateFailsADeterministicFraction(t *testing.T) {
	t.Parallel()

	s := New().Add(Rule{Path: "/x*", Rate: 0.2})
	var failedAt []int
	for i := 1; i <= 10; i++ {
		if _, ok := s.Match("", "/x/req"); ok {
			failedAt = append(failedAt, i)
		}
	}
	want := []int{5, 10}
	if len(failedAt) != len(want) || failedAt[0] != want[0] || failedAt[1] != want[1] {
		t.Errorf("failed at %v, want %v (exactly 20%%, evenly spaced)", failedAt, want)
	}
}

// TestFailsRequest holds the latency-only distinction: a bare rule and an error
// rule fail the request; a latency-only rule slows it but lets it succeed.
func TestFailsRequest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		rule Rule
		want bool
	}{
		{"bare", Rule{Path: "/x*"}, true},
		{"error", Rule{Path: "/x*", Status: 500}, true},
		{"latency only", Rule{Path: "/x*", Latency: time.Second}, false},
		{"latency and error", Rule{Path: "/x*", Latency: time.Second, Status: 500}, true},
	}
	for _, tc := range cases {
		if got := tc.rule.FailsRequest(); got != tc.want {
			t.Errorf("%s: FailsRequest = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func passthrough(context.Context, any) (any, error) { return "ok", nil }

// TestUnaryInterceptorMapsToGRPCCode holds that a gRPC fault returns a real
// gRPC status with the code the HTTP status maps to — 500 becomes Internal, not
// an HTTP number smuggled over gRPC — and that a non-matching method is
// untouched.
func TestUnaryInterceptorMapsToGRPCCode(t *testing.T) {
	t.Parallel()

	s := New().Add(Rule{Path: "/google.pubsub.v1.*", Status: 500})
	intercept := s.UnaryServerInterceptor(clock.NewFake(faultEpoch))

	_, err := intercept(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/google.pubsub.v1.Publisher/Publish"}, passthrough)
	if status.Code(err) != codes.Internal {
		t.Errorf("code = %v, want Internal for a 500 fault", status.Code(err))
	}

	out, err := intercept(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/google.firestore.v1.Firestore/GetDocument"}, passthrough)
	if err != nil || out != "ok" {
		t.Errorf("a non-matching method was faulted: out=%v err=%v", out, err)
	}
}

// TestUnaryInterceptorLatencyRunsOnClock holds that an injected latency waits on
// the injected clock — the call blocks until time advances — and that a
// latency-only fault then lets the real handler run.
func TestUnaryInterceptorLatencyRunsOnClock(t *testing.T) {
	t.Parallel()

	clk := clock.NewFake(faultEpoch)
	s := New().Add(Rule{Path: "/x*", Latency: time.Hour})
	intercept := s.UnaryServerInterceptor(clk)

	type result struct {
		out any
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := intercept(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/x/M"}, passthrough)
		done <- result{out, err}
	}()

	select {
	case <-done:
		t.Fatal("returned before the injected latency elapsed")
	case <-time.After(50 * time.Millisecond):
	}

	clk.Advance(time.Hour)
	select {
	case r := <-done:
		if r.err != nil || r.out != "ok" {
			t.Errorf("latency-only fault did not succeed after the delay: out=%v err=%v", r.out, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not return after the clock advanced past the latency")
	}
}
