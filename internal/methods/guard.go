package methods

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc"
)

// ErrUnregistered means an RPC was attempted that the registry does not list.
//
// It is a programming error, not an operational one, and it is deliberately
// fatal to the call. The alternative is worse: the call succeeds in development
// against admin.macaroon and fails in production against the baked credential,
// at whatever point in the sequence it happens to sit.
var ErrUnregistered = errors.New("this build calls an LND method that internal/methods does not list")

// UnaryGuard refuses any unary call to an unregistered method.
//
// It checks membership only, not Use: the harness dials through lnd.Dial too,
// and narrowing production to InApp is the baked macaroon's job — LND enforces
// that, and it does so whether or not this process agrees. What the guard adds
// is the thing the macaroon cannot give: a failure that names the registry.
func UnaryGuard() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any,
		cc *grpc.ClientConn, invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption) error {

		if err := check(method); err != nil {
			return err
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// StreamGuard is UnaryGuard for streaming calls. OpenChannel is one, so this is
// not decoration.
func StreamGuard() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn,
		method string, streamer grpc.Streamer,
		opts ...grpc.CallOption) (grpc.ClientStream, error) {

		if err := check(method); err != nil {
			return nil, err
		}
		return streamer(ctx, desc, cc, method, opts...)
	}
}

func check(method string) error {
	if _, ok := Lookup(method); ok {
		return nil
	}
	if isForbidden(method) {
		return fmt.Errorf("%w: %s is on the never-list — the credential this "+
			"tool bakes must not be able to %s, so this call could not have "+
			"worked in production either",
			ErrUnregistered, method, capabilityOf(method))
	}
	return fmt.Errorf("%w: %s. Add it to internal/methods with the call site "+
		"and LND's own permission for it, then re-bake the macaroon with "+
		"`winthistle print-macaroon-command`", ErrUnregistered, method)
}
