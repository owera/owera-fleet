// Package ssh is the typed SSH client for owera-fleet.
//
// It replaces lib/ssh.sh's bash wrappers around the system ssh binary with
// a pure-Go client built on golang.org/x/crypto/ssh. The two non-trivial
// design choices:
//
//  1. Authentication. The plan calls for "Keychain integration"; on macOS
//     that means the user's ssh-agent already holds the dedicated key
//     (Keychain-backed via `ssh-add --apple-use-keychain`). We delegate to
//     the agent over SSH_AUTH_SOCK rather than re-parsing the key
//     ourselves, so passphrases never leave the agent. Bootstrap's
//     "initial mode" — when the dedicated key does not yet exist — passes
//     an explicit keyfile via [WithKeyFile]; that path is used exactly
//     once, until [internal/bootstrap]'s Phase 6 lands the dedicated key
//     and subsequent calls switch back to agent auth.
//
//  2. Host-key handling. lib/ssh.sh used
//     `StrictHostKeyChecking=accept-new`: trust on first sight, then
//     verify on subsequent connects. We replicate that exactly here so
//     fresh workers (whose host key is not yet in ~/.ssh/known_hosts) work
//     on first bootstrap, without weakening verification afterwards.
//     [WithStrictHostKey] flips this off for callers that want a hard
//     fail on unknown hosts.
//
// The package is dependency-injectable: a Dialer struct holds clock,
// agent socket, known-hosts path, and dial function so tests substitute
// in-memory fakes without poking the network.
package ssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// DefaultPort is the SSH port assumed when [Target] does not carry one.
const DefaultPort = 22

// DefaultConnectTimeout matches lib/ssh.sh's "-o ConnectTimeout=10".
const DefaultConnectTimeout = 10 * time.Second

// DefaultServerAliveInterval matches lib/ssh.sh's "-o ServerAliveInterval=30".
const DefaultServerAliveInterval = 30 * time.Second

// DefaultKnownHostsPath is the conventional macOS / Linux location.
// Resolution happens lazily so tests can override $HOME.
const DefaultKnownHostsPath = "~/.ssh/known_hosts"

// Target represents a user@host[:port] destination.
type Target struct {
	User string
	Host string
	Port int
}

// String returns "user@host:port".
func (t Target) String() string {
	return fmt.Sprintf("%s@%s:%d", t.User, t.Host, t.Port)
}

// Address is the host:port form used by net.Dial.
func (t Target) Address() string {
	return net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
}

// ParseTarget accepts "user@host" or "user@host:port" and fills in
// [DefaultPort] when the port is omitted. Empty user, empty host, or a
// non-integer port returns an error keyed for human-readable display.
func ParseTarget(raw string) (Target, error) {
	raw = strings.TrimSpace(raw)
	at := strings.IndexByte(raw, '@')
	if at <= 0 || at == len(raw)-1 {
		return Target{}, fmt.Errorf("ssh: target %q must be user@host[:port]", raw)
	}
	user := strings.TrimSpace(raw[:at])
	hostPort := strings.TrimSpace(raw[at+1:])
	if user == "" {
		return Target{}, fmt.Errorf("ssh: empty user in target %q", raw)
	}
	host := hostPort
	port := DefaultPort
	if strings.LastIndexByte(hostPort, ':') > 0 {
		h, p, err := net.SplitHostPort(hostPort)
		if err != nil {
			return Target{}, fmt.Errorf("ssh: target %q: %w", raw, err)
		}
		host = h
		n, perr := strconv.Atoi(p)
		if perr != nil || n <= 0 || n > 65535 {
			return Target{}, fmt.Errorf("ssh: target %q: invalid port %q", raw, p)
		}
		port = n
	}
	if host == "" {
		return Target{}, fmt.Errorf("ssh: empty host in target %q", raw)
	}
	return Target{User: user, Host: host, Port: port}, nil
}

// Config controls one Dial / Run cycle. Most callers use the Option
// constructors below rather than building this struct by hand.
type Config struct {
	ConnectTimeout      time.Duration
	ServerAliveInterval time.Duration
	KnownHostsPath      string
	AcceptNew           bool
	StrictHostKey       bool
	AgentSocket         string
	KeyFile             string
	KeyPassphrase       []byte

	// AllowLinkLocal disables the default behavior of filtering IPv6
	// link-local (fe80::/10) and site-local (fec0::/10) addresses out
	// of resolver results before dialing. See [WithAllowLinkLocal].
	AllowLinkLocal bool
}

// Option mutates a Config; used in variadic Dial/Run calls.
type Option func(*Config)

// WithConnectTimeout overrides [DefaultConnectTimeout].
func WithConnectTimeout(d time.Duration) Option {
	return func(c *Config) { c.ConnectTimeout = d }
}

// WithServerAliveInterval overrides [DefaultServerAliveInterval]. Set to
// zero to disable the keepalive entirely.
func WithServerAliveInterval(d time.Duration) Option {
	return func(c *Config) { c.ServerAliveInterval = d }
}

// WithKnownHostsPath overrides [DefaultKnownHostsPath]. "~/" is expanded
// against the operator's home directory.
func WithKnownHostsPath(path string) Option {
	return func(c *Config) { c.KnownHostsPath = path }
}

// WithAcceptNew (default true) lets the dialer accept-and-append unseen
// host keys, mirroring "-o StrictHostKeyChecking=accept-new".
func WithAcceptNew(accept bool) Option {
	return func(c *Config) { c.AcceptNew = accept }
}

// WithStrictHostKey flips off [WithAcceptNew] and rejects any host whose
// key is not already in known_hosts.
func WithStrictHostKey() Option {
	return func(c *Config) { c.StrictHostKey = true; c.AcceptNew = false }
}

// WithAgentSocket overrides $SSH_AUTH_SOCK. Intended for tests and for
// pinning to a non-default agent (e.g. a sandboxed forwarded agent).
func WithAgentSocket(path string) Option {
	return func(c *Config) { c.AgentSocket = path }
}

// WithKeyFile points the dialer at an explicit private-key file. Used by
// bootstrap's initial mode, before the dedicated key exists.
//
// passphrase may be nil for unencrypted keys; never log it.
func WithKeyFile(path string, passphrase []byte) Option {
	return func(c *Config) {
		c.KeyFile = path
		c.KeyPassphrase = append([]byte(nil), passphrase...)
	}
}

// WithAllowLinkLocal disables the default IPv6 link-local / site-local
// filter applied to resolver results before dialing. The filter exists
// to avoid the long-running-daemon failure mode in issue #42, where a
// stale fe80:: neighbor cache entry kept the heartbeats-bridge dialing
// an unreachable address for hours. Production callers should leave the
// filter on; tests and unusual topologies (single-segment IPv6 link
// where workers really are addressed via fe80::) may opt out.
func WithAllowLinkLocal() Option {
	return func(c *Config) { c.AllowLinkLocal = true }
}

// resolverFunc resolves a hostname to a slice of [net.IPAddr]. It
// matches the signature of [net.Resolver.LookupIPAddr] so tests can
// inject fakes without poking the network.
type resolverFunc func(ctx context.Context, host string) ([]net.IPAddr, error)

// Dialer is the typed entry point. Construct one per session; concurrent
// Dial calls are safe.
type Dialer struct {
	cfg Config

	// dialContext exists for tests; production wraps net.Dial. It is
	// invoked once per filtered IP candidate from [Dialer.dialFiltered].
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// resolver exists for tests; production wraps net.DefaultResolver.
	// LookupIPAddr. See [resolverFunc] for the signature.
	resolver resolverFunc
	// resolveHome exists for tests.
	resolveHome func() (string, error)
	// agentLookup exists for tests; production opens SSH_AUTH_SOCK.
	agentLookup func(socket string) (agent.Agent, io.Closer, error)
	// now is the clock; tests substitute a frozen clock.
	now func() time.Time
}

// NewDialer returns a Dialer with defaults applied; pass Options to tune.
func NewDialer(opts ...Option) *Dialer {
	cfg := Config{
		ConnectTimeout:      DefaultConnectTimeout,
		ServerAliveInterval: DefaultServerAliveInterval,
		KnownHostsPath:      DefaultKnownHostsPath,
		AcceptNew:           true,
		AgentSocket:         os.Getenv("SSH_AUTH_SOCK"),
	}
	for _, o := range opts {
		o(&cfg)
	}
	return &Dialer{
		cfg: cfg,
		dialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
		resolver: func(ctx context.Context, host string) ([]net.IPAddr, error) {
			return net.DefaultResolver.LookupIPAddr(ctx, host)
		},
		resolveHome: os.UserHomeDir,
		agentLookup: openAgent,
		now:         time.Now,
	}
}

// Config returns a copy of the dialer's effective config. Read-only.
func (d *Dialer) Config() Config {
	out := d.cfg
	if out.KeyPassphrase != nil {
		out.KeyPassphrase = append([]byte(nil), out.KeyPassphrase...)
	}
	return out
}

// Client is one live SSH connection. Sessions over the same Client are
// cheap; opening a new Client is not. Reuse the Client for repeated
// commands against the same node.
type Client struct {
	target Target
	conn   *cryptossh.Client

	mu      sync.Mutex
	closers []io.Closer
	closed  bool
}

// Target returns the user@host:port this client connects to.
func (c *Client) Target() Target { return c.target }

// Result is the structured outcome of a remote command.
type Result struct {
	Stdout     string
	Stderr     string
	ExitCode   int
	DurationMs int64
}

// MaxCapturedStderr caps how much stderr is held in memory per call. The
// limit mirrors the 4 KiB tail [lib/ssh.sh] used in `run_remote`.
const MaxCapturedStderr = 4096

// Dial opens a connection to t with the dialer's effective config plus
// any per-call Options. Per-call Options shadow defaults but do not
// mutate the dialer.
//
// Name resolution for non-literal targets goes through [Dialer.dialFiltered],
// which filters link-local candidates (#42) and tolerates the residual
// intermittent ULA-only resolver responses documented on that method
// (#44). Callers should expect occasional transient dial failures on
// .local mDNS hosts and retry on their own cadence.
func (d *Dialer) Dial(ctx context.Context, t Target, opts ...Option) (*Client, error) {
	cfg := d.cfg
	for _, o := range opts {
		o(&cfg)
	}

	auth, agentCloser, err := d.buildAuth(cfg)
	if err != nil {
		return nil, err
	}

	hkCallback, err := d.buildHostKeyCallback(cfg, t)
	if err != nil {
		if agentCloser != nil {
			_ = agentCloser.Close()
		}
		return nil, err
	}

	clientCfg := &cryptossh.ClientConfig{
		User:            t.User,
		Auth:            auth,
		HostKeyCallback: hkCallback,
		Timeout:         cfg.ConnectTimeout,
	}

	dialCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()

	rawConn, err := d.dialFiltered(dialCtx, "tcp", t.Address(), cfg)
	if err != nil {
		if agentCloser != nil {
			_ = agentCloser.Close()
		}
		return nil, fmt.Errorf("ssh: dial %s: %w", t.Address(), err)
	}

	conn, chans, reqs, err := cryptossh.NewClientConn(rawConn, t.Address(), clientCfg)
	if err != nil {
		_ = rawConn.Close()
		if agentCloser != nil {
			_ = agentCloser.Close()
		}
		return nil, fmt.Errorf("ssh: handshake %s: %w", t, err)
	}

	c := cryptossh.NewClient(conn, chans, reqs)
	client := &Client{target: t, conn: c}
	if agentCloser != nil {
		client.closers = append(client.closers, agentCloser)
	}

	if cfg.ServerAliveInterval > 0 {
		client.startKeepalive(cfg.ServerAliveInterval)
	}
	return client, nil
}

// dialFiltered resolves addr to its IP candidates, filters out IPv6
// link-local (fe80::/10) and site-local (fec0::/10) entries unless
// cfg.AllowLinkLocal is set, and dials the survivors in resolver order
// until one succeeds. Returns a clear "no non-link-local address"
// error when every resolver result was filtered out — operators can
// grep logs for "link-local" to attribute the failure mode in #42.
//
// The filter exists because Go's resolver (cgo getaddrinfo on darwin)
// returns stale fe80:: entries for .local mDNS hostnames after the
// neighbor cache rotates, and a long-running process holds onto them
// forever. Filtering up front means recovery is automatic on the next
// dial instead of requiring a process restart.
//
// Literal IP addresses (no resolver round-trip) and addresses that
// fail to parse as host:port are passed through to the underlying
// dialer untouched — they're operator-supplied, not resolver-stale.
//
// # Residual: ULA-only resolver responses (#44)
//
// On darwin's cgo resolver path, .local mDNS lookups intermittently
// return only the IPv6 ULA address (fd00::/8) for a host even when the
// system normally has IPv4 + global IPv6 + ULA candidates (dscacheutil
// returns the full set on the same lookup). When that happens
// dialFiltered has only the ULA to try; if the LAN doesn't route ULA
// (a common misconfiguration), the dial fails and the caller must
// retry on its own schedule.
//
// Long-running daemons (heartbeats-bridge) self-heal within one poll
// cycle (~60s) because the resolver typically returns a fuller answer
// next time, and the watchdog's stale-heartbeat threshold (5min) gives
// plenty of margin before any alert. If the failure rate ever becomes
// operationally annoying, revisit owera-fleet issue #44 — it
// enumerates four alternative fixes (force pure-Go resolver, filter
// ULA, intra-poll re-resolve, parallel happy-eyeballs). The current
// posture is Option E: accept + document.
func (d *Dialer) dialFiltered(ctx context.Context, network, addr string, cfg Config) (net.Conn, error) {
	if cfg.AllowLinkLocal {
		return d.dialContext(ctx, network, addr)
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// Malformed addr — pass through; the underlying dialer will
		// emit the canonical error.
		return d.dialContext(ctx, network, addr)
	}

	// Literal IPs bypass the resolver and the filter. An operator who
	// explicitly types an fe80:: address knows what they're doing; we
	// only guard against resolver-supplied staleness.
	if ip := net.ParseIP(host); ip != nil {
		return d.dialContext(ctx, network, addr)
	}

	ips, err := d.resolver(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}

	usable := make([]net.IPAddr, 0, len(ips))
	for _, ip := range ips {
		if isFilteredIP(ip.IP) {
			continue
		}
		usable = append(usable, ip)
	}
	if len(usable) == 0 {
		return nil, fmt.Errorf("no non-link-local address for %s (resolver returned only link-local/site-local addresses; long-running daemon may need a restart if this is a stale neighbor cache — see issue #42)", host)
	}

	// Iterate candidates in resolver order. The system resolver already
	// applies its own preference policy (typically IPv4-first when both
	// are present on darwin); we preserve that ordering. First successful
	// dial wins; otherwise return the last error so the operator sees
	// the underlying transport problem, not a generic "all failed".
	var lastErr error
	for _, ip := range usable {
		candidate := net.JoinHostPort(ipWithZone(ip), port)
		conn, derr := d.dialContext(ctx, network, candidate)
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	return nil, lastErr
}

// isFilteredIP reports whether ip is an address the dialer should drop
// before attempting to connect. The set is IPv6 link-local unicast
// (fe80::/10), IPv6 link-local multicast (ff02::/16 and friends), IPv4
// link-local (169.254.0.0/16, also covered by IsLinkLocalUnicast), and
// IPv6 site-local (fec0::/10, formally deprecated by RFC 3879 but still
// occasionally emitted by misconfigured resolvers; cheap to filter).
func isFilteredIP(ip net.IP) bool {
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	// fec0::/10: IPv6 site-local. Not covered by IsLinkLocalUnicast.
	if v6 := ip.To16(); v6 != nil && ip.To4() == nil {
		if v6[0] == 0xfe && (v6[1]&0xc0) == 0xc0 {
			return true
		}
	}
	return false
}

// ipWithZone renders ip's textual form, preserving any IPv6 zone-id
// (e.g. "fe80::1%en0"). net.IPAddr.String() already does this; we
// expose it as a tiny shim so the dial loop reads top-to-bottom.
func ipWithZone(ip net.IPAddr) string { return ip.String() }

// Run executes cmd remotely, returning structured Stdout/Stderr/ExitCode.
//
// Stderr is captured up to [MaxCapturedStderr] from the tail (newest
// bytes preserved). A non-zero exit code is *not* an error — callers
// check Result.ExitCode for that and reserve err for transport problems
// (network, signal, malformed reply).
func (c *Client) Run(ctx context.Context, cmd string) (Result, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return Result{}, ErrClosed
	}
	c.mu.Unlock()

	start := time.Now()
	sess, err := c.conn.NewSession()
	if err != nil {
		return Result{}, fmt.Errorf("ssh: new session %s: %w", c.target, err)
	}
	defer sess.Close()

	var stdout, stderr strings.Builder
	sess.Stdout = &stdout
	tail := newTailWriter(MaxCapturedStderr)
	sess.Stderr = io.MultiWriter(&stderr, tail)

	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()

	select {
	case err = <-done:
	case <-ctx.Done():
		_ = sess.Signal(cryptossh.SIGTERM)
		_ = sess.Close()
		<-done // drain
		return Result{
			Stdout:     stdout.String(),
			Stderr:     tail.String(),
			ExitCode:   -1,
			DurationMs: time.Since(start).Milliseconds(),
		}, ctx.Err()
	}

	res := Result{
		Stdout:     stdout.String(),
		Stderr:     tail.String(),
		ExitCode:   0,
		DurationMs: time.Since(start).Milliseconds(),
	}

	if err != nil {
		var exitErr *cryptossh.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitStatus()
			return res, nil
		}
		var missingErr *cryptossh.ExitMissingError
		if errors.As(err, &missingErr) {
			res.ExitCode = -1
			return res, fmt.Errorf("ssh: remote exit code missing: %w", err)
		}
		return res, fmt.Errorf("ssh: run %s: %w", c.target, err)
	}
	return res, nil
}

// RunCapture is a stdout-only convenience for probes. The remote command
// must exit 0; non-zero exit is converted into an error tagged with the
// captured stderr tail.
func (c *Client) RunCapture(ctx context.Context, cmd string) (string, error) {
	res, err := c.Run(ctx, cmd)
	if err != nil {
		return res.Stdout, err
	}
	if res.ExitCode != 0 {
		return res.Stdout, fmt.Errorf("ssh: %s exit %d: %s", c.target, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

// Close releases the connection plus any auxiliary closers (agent socket,
// host-key reload handles). Idempotent.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	var firstErr error
	if c.conn != nil {
		if err := c.conn.Close(); err != nil && !errors.Is(err, io.EOF) {
			firstErr = err
		}
		c.conn = nil
	}
	for _, cl := range c.closers {
		if err := cl.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	c.closers = nil
	return firstErr
}

// ErrClosed is returned by [Client.Run] after [Client.Close].
var ErrClosed = errors.New("ssh: client closed")

func (c *Client) startKeepalive(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			c.mu.Lock()
			closed := c.closed
			conn := c.conn
			c.mu.Unlock()
			if closed || conn == nil {
				return
			}
			_, _, err := conn.SendRequest("keepalive@owera-fleet", true, nil)
			if err != nil {
				return
			}
		}
	}()
}

// buildAuth assembles the AuthMethod slice. Order matches lib/ssh.sh:
// when both a keyfile and an agent are present, the keyfile wins
// (initial-mode bootstrap), with the agent as a fallback so re-runs after
// Phase 6 still succeed via the dedicated key.
func (d *Dialer) buildAuth(cfg Config) ([]cryptossh.AuthMethod, io.Closer, error) {
	var methods []cryptossh.AuthMethod

	if cfg.KeyFile != "" {
		signer, err := loadSigner(cfg.KeyFile, cfg.KeyPassphrase)
		if err != nil {
			return nil, nil, err
		}
		methods = append(methods, cryptossh.PublicKeys(signer))
	}

	var agentCloser io.Closer
	if cfg.AgentSocket != "" {
		ag, closer, err := d.agentLookup(cfg.AgentSocket)
		if err == nil {
			methods = append(methods, cryptossh.PublicKeysCallback(ag.Signers))
			agentCloser = closer
		}
		// Silent fallback when the agent socket is unreachable: the key
		// file path may still work, and a missing agent on a CI runner
		// shouldn't break test wiring.
	}

	if len(methods) == 0 {
		return nil, nil, errors.New("ssh: no auth methods available (set SSH_AUTH_SOCK or pass WithKeyFile)")
	}
	return methods, agentCloser, nil
}

func (d *Dialer) buildHostKeyCallback(cfg Config, t Target) (cryptossh.HostKeyCallback, error) {
	path, err := d.expandHome(cfg.KnownHostsPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("ssh: mkdir known_hosts parent: %w", err)
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if cfg.StrictHostKey {
			return nil, fmt.Errorf("ssh: strict host key required but %s missing", path)
		}
		if err := os.WriteFile(path, []byte{}, 0o600); err != nil {
			return nil, fmt.Errorf("ssh: seed known_hosts %s: %w", path, err)
		}
	}

	strict, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("ssh: load known_hosts %s: %w", path, err)
	}

	if cfg.StrictHostKey {
		return strict, nil
	}

	return func(hostname string, remote net.Addr, key cryptossh.PublicKey) error {
		if err := strict(hostname, remote, key); err == nil {
			return nil
		} else {
			var ke *knownhosts.KeyError
			if !errors.As(err, &ke) || len(ke.Want) > 0 {
				return err
			}
		}
		if !cfg.AcceptNew {
			return fmt.Errorf("ssh: host %s not in %s", t.Host, path)
		}
		entry := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("ssh: append known_hosts: %w", err)
		}
		defer f.Close()
		if _, err := fmt.Fprintln(f, entry); err != nil {
			return fmt.Errorf("ssh: write known_hosts entry: %w", err)
		}
		return nil
	}, nil
}

func (d *Dialer) expandHome(p string) (string, error) {
	if !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := d.resolveHome()
	if err != nil {
		return "", fmt.Errorf("ssh: resolve home: %w", err)
	}
	return filepath.Join(home, p[2:]), nil
}

// openAgent dials the ssh-agent over the unix socket at path.
// On success the returned Closer should be called when the connection is
// no longer needed; closing the socket also tears down any ongoing
// PublicKeysCallback queries.
func openAgent(path string) (agent.Agent, io.Closer, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, nil, fmt.Errorf("ssh: dial agent %s: %w", path, err)
	}
	return agent.NewClient(conn), conn, nil
}

func loadSigner(path string, passphrase []byte) (cryptossh.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ssh: read key %s: %w", path, err)
	}
	if len(passphrase) == 0 {
		return cryptossh.ParsePrivateKey(data)
	}
	return cryptossh.ParsePrivateKeyWithPassphrase(data, passphrase)
}

// tailWriter is an io.Writer that retains only the trailing N bytes.
type tailWriter struct {
	cap int
	buf []byte
}

func newTailWriter(cap int) *tailWriter { return &tailWriter{cap: cap} }

func (t *tailWriter) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.cap {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.cap:]...)
	}
	return len(p), nil
}

func (t *tailWriter) String() string { return string(t.buf) }
