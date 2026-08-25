// Package lnd holds the gRPC client Winthistle uses to drive LND.
//
// The credential is deliberately narrow: see `winthistle print-macaroon-command`
// for the exact method list this build calls. Nothing here bakes or widens a
// macaroon — it only presents one.
package lnd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/AusDavo/winthistle/internal/methods"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Config is the [lnd] block of winthistle.toml: an address and two paths. It
// never carries a secret itself, only the location of one.
type Config struct {
	Address  string // host:port of LND's gRPC listener
	TLSCert  string // path to tls.cert
	Macaroon string // path to the baked macaroon — not admin.macaroon
}

// Client is a connection to one LND node. Both sub-clients ride the same
// connection, so Close covers them together.
type Client struct {
	conn      *grpc.ClientConn
	Lightning lnrpc.LightningClient
	WalletKit walletrpc.WalletKitClient
}

// macaroonCreds attaches a hex-encoded macaroon to every call.
//
// Hand-rolled rather than using lnd/macaroons, which drags in kvdb and etcd for
// the sake of fifteen lines. RequireTransportSecurity is true: gRPC then refuses
// to send the credential over a plaintext connection, so a misconfigured address
// fails loudly instead of leaking it.
type macaroonCreds struct{ hexMac string }

func (m macaroonCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"macaroon": m.hexMac}, nil
}

func (m macaroonCreds) RequireTransportSecurity() bool { return true }

// Dial connects to LND and does not return until a GetInfo has round-tripped, so
// a bad address or an unreadable credential surfaces here rather than at the
// first call inside a timed window. The gRPC connection itself is lazy; the
// probe is what makes this blocking.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	certBytes, err := os.ReadFile(cfg.TLSCert)
	if err != nil {
		return nil, fmt.Errorf("reading tls cert %s: %w", cfg.TLSCert, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certBytes) {
		return nil, fmt.Errorf("no certificate found in %s", cfg.TLSCert)
	}

	macBytes, err := os.ReadFile(cfg.Macaroon)
	if err != nil {
		return nil, fmt.Errorf("reading macaroon %s: %w", cfg.Macaroon, err)
	}

	// LND's self-signed cert is the trust root; MinVersion is set because the
	// default floor has moved before and this is not a place to inherit one.
	tlsCfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	// NewClient rather than DialContext. This used to be DialContext because we
	// held lnd's own grpc pin at v1.59, where NewClient does not exist; lnd
	// v0.21.2-beta pins v1.79, where DialContext is deprecated. Nothing is lost
	// in the swap: DialContext without WithBlock never dialled either, so the
	// 15-second context it was given bounded nothing. The GetInfo probe below is
	// the only thing here that has ever proved the connection works, and it is
	// still the thing that fails on a wrong address.
	//
	// One real difference: NewClient resolves through the dns resolver where
	// DialContext defaulted to passthrough. [lnd] address is a host:port, which
	// both handle, and a literal IP short-circuits dns — so this changes nothing
	// for any address config.Load accepts.
	//
	// The guards refuse any call to a method internal/methods does not list, on
	// both the unary and the streaming path. They are not a substitute for the
	// baked macaroon — LND enforces that, and it does so whether or not this
	// process agrees — but they turn a call site that outran the registry into a
	// loud, local failure instead of one that works here and fails in production.
	conn, err := grpc.NewClient(cfg.Address,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithPerRPCCredentials(macaroonCreds{hex.EncodeToString(macBytes)}),
		grpc.WithChainUnaryInterceptor(methods.UnaryGuard()),
		grpc.WithChainStreamInterceptor(methods.StreamGuard()),
	)
	if err != nil {
		return nil, fmt.Errorf("dialing lnd at %s: %w", cfg.Address, err)
	}

	c := &Client{
		conn:      conn,
		Lightning: lnrpc.NewLightningClient(conn),
		WalletKit: walletrpc.NewWalletKitClient(conn),
	}

	// One cheap call to prove address, certificate and macaroon all work
	// together. GetInfo is in the permission list precisely for this.
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := c.Lightning.GetInfo(probeCtx, &lnrpc.GetInfoRequest{}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("lnd at %s did not answer GetInfo: %w", cfg.Address, err)
	}
	return c, nil
}

// Close releases the connection.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}
