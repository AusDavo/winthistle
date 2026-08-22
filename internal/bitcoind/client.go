// Package bitcoind is a small JSON-RPC client for Bitcoin Core.
//
// Hand-rolled rather than pulled in: the app needs about eight methods, several
// of them descriptor-wallet calls that general-purpose Go clients cover
// unevenly, and the credentials involved are worth being able to read end to end.
package bitcoind

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Config is the [bitcoind] block of winthistle.toml. Cookie is preferred; User
// and Pass exist because a pruned-node operator may already be running with
// rpcauth and no cookie file.
type Config struct {
	Address string // host:port
	Cookie  string // path to .cookie; takes precedence over User/Pass
	User    string
	Pass    string
	Wallet  string // wallet name — the watch-only descriptor wallet

	// Timeout bounds one HTTP round trip. It defaults to DefaultTimeout, which
	// is ample for every call this app makes except one: importdescriptors
	// blocks for the whole rescan, minutes to hours on mainnet. Setup builds its
	// own client with a timeout that covers that; nothing else should need to.
	Timeout time.Duration
}

// DefaultTimeout is the HTTP timeout a client gets when Config.Timeout is zero.
const DefaultTimeout = 2 * time.Minute

// Client talks to one Core wallet.
type Client struct {
	url  string
	user string
	pass string
	http *http.Client
}

// Error is a JSON-RPC error from Core, kept whole so callers can match on Code
// rather than on message text.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("bitcoind rpc error %d: %s", e.Code, e.Message) }

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type response struct {
	Result json.RawMessage `json:"result"`
	Error  *Error          `json:"error"`
}

// New builds a client. Wallet-scoped calls go to /wallet/<name>, so one client
// is bound to one wallet — deliberate, since sending a wallet call to the
// default endpoint on a multi-wallet node fails in a confusing way.
func New(cfg Config) (*Client, error) {
	user, pass := cfg.User, cfg.Pass
	if cfg.Cookie != "" {
		raw, err := os.ReadFile(cfg.Cookie)
		if err != nil {
			return nil, fmt.Errorf("reading cookie %s: %w", cfg.Cookie, err)
		}
		u, p, ok := strings.Cut(strings.TrimSpace(string(raw)), ":")
		if !ok {
			return nil, fmt.Errorf("cookie %s is not user:pass", cfg.Cookie)
		}
		user, pass = u, p
	}
	if user == "" {
		return nil, fmt.Errorf("bitcoind: no cookie path and no rpc user configured")
	}

	url := "http://" + cfg.Address
	if cfg.Wallet != "" {
		url += "/wallet/" + cfg.Wallet
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{
		url:  url,
		user: user,
		pass: pass,
		http: &http.Client{Timeout: timeout},
	}, nil
}

// Call issues one JSON-RPC call and unmarshals result into out. out may be nil
// when the result is not needed.
func (c *Client) Call(ctx context.Context, method string, params []any, out any) error {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(request{JSONRPC: "1.0", ID: "winthistle", Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("encoding %s request: %w", method, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building %s request: %w", method, err)
	}
	req.SetBasicAuth(c.user, c.pass)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("calling %s: %w", method, err)
	}
	defer resp.Body.Close()

	var parsed response
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&parsed); err != nil {
		// A non-JSON body means an HTTP-level failure — wrong credentials or a
		// wallet that is not loaded both land here.
		return fmt.Errorf("calling %s: http %s, unparseable body: %w", method, resp.Status, err)
	}
	if parsed.Error != nil {
		return fmt.Errorf("%s: %w", method, parsed.Error)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("calling %s: http %s", method, resp.Status)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(parsed.Result, out); err != nil {
		return fmt.Errorf("decoding %s result: %w", method, err)
	}
	return nil
}
