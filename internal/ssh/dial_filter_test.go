package ssh

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
)

// dialAttempt records one call into the fake dialContext so tests can
// assert which candidate addresses the filter actually attempted.
type dialAttempt struct {
	network, addr string
}

// newCaptureDialer returns a Dialer wired with an in-memory resolver
// (returning ips) and a fake dialContext that records every attempt.
// The fake dialContext returns dialErr when the candidate matches
// failOn, otherwise net.ErrClosed by default so the caller sees the
// attempt without us having to spin up a real listener.
func newCaptureDialer(t *testing.T, ips []net.IPAddr, succeedOn string) (*Dialer, *[]dialAttempt, *sync.Mutex) {
	t.Helper()
	d := NewDialer(WithAgentSocket("")) // agent doesn't matter for dialFiltered
	var (
		mu       sync.Mutex
		attempts []dialAttempt
	)
	d.resolver = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return ips, nil
	}
	d.dialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		attempts = append(attempts, dialAttempt{network, addr})
		mu.Unlock()
		if succeedOn != "" && strings.Contains(addr, succeedOn) {
			// Return a closed pipe so the caller has a usable net.Conn
			// without us holding a real socket open.
			c1, c2 := net.Pipe()
			_ = c2.Close()
			return c1, nil
		}
		return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("no route to host")}
	}
	return d, &attempts, &mu
}

func TestDialFiltered_FiltersLinkLocalIPv6(t *testing.T) {
	// Resolver returns one fe80:: (link-local) and one IPv4. The dial
	// must NOT attempt fe80::; it must dial 192.168.1.1.
	ips := []net.IPAddr{
		{IP: net.ParseIP("fe80::8c2:3fe6:b612:8749"), Zone: "en0"},
		{IP: net.ParseIP("192.168.1.1")},
	}
	d, attempts, mu := newCaptureDialer(t, ips, "192.168.1.1")
	conn, err := d.dialFiltered(context.Background(), "tcp", "claw1.local:22", d.cfg)
	if err != nil {
		t.Fatalf("dialFiltered: %v", err)
	}
	defer conn.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(*attempts) != 1 {
		t.Fatalf("expected exactly 1 dial attempt, got %d: %+v", len(*attempts), *attempts)
	}
	got := (*attempts)[0].addr
	if !strings.Contains(got, "192.168.1.1") {
		t.Errorf("dial addr = %q, want it to contain 192.168.1.1", got)
	}
	if strings.Contains(got, "fe80") {
		t.Errorf("dial addr = %q, must not include fe80:: link-local", got)
	}
}

func TestDialFiltered_OnlyLinkLocalReturnsClearError(t *testing.T) {
	// Resolver returns ONLY link-local — the bug condition from #42.
	// The dialer must not attempt the dial at all, and the returned
	// error must mention "link-local" so log-greppers find it.
	ips := []net.IPAddr{
		{IP: net.ParseIP("fe80::8c2:3fe6:b612:8749"), Zone: "en0"},
	}
	d, attempts, mu := newCaptureDialer(t, ips, "")
	_, err := d.dialFiltered(context.Background(), "tcp", "claw1.local:22", d.cfg)
	if err == nil {
		t.Fatal("expected error when only link-local addresses are available")
	}
	if !strings.Contains(err.Error(), "link-local") {
		t.Errorf("error %q should mention 'link-local' for log grepping", err)
	}
	if !strings.Contains(err.Error(), "claw1.local") {
		t.Errorf("error %q should mention the host so operators know which dial failed", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*attempts) != 0 {
		t.Errorf("dialContext must not be called when all candidates are filtered; got %d attempts", len(*attempts))
	}
}

func TestDialFiltered_AllowLinkLocalEscapeHatch(t *testing.T) {
	// With WithAllowLinkLocal, the filter is bypassed entirely: the
	// dialer hands the original host:port string straight to the
	// underlying dialContext, no resolver round-trip.
	d := NewDialer(WithAgentSocket(""), WithAllowLinkLocal())
	var got string
	d.resolver = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		t.Fatalf("resolver must not be called when AllowLinkLocal is set; host=%s", host)
		return nil, nil
	}
	d.dialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		got = addr
		c1, c2 := net.Pipe()
		_ = c2.Close()
		return c1, nil
	}
	conn, err := d.dialFiltered(context.Background(), "tcp", "[fe80::1%en0]:22", d.cfg)
	if err != nil {
		t.Fatalf("dialFiltered with AllowLinkLocal: %v", err)
	}
	defer conn.Close()
	if got != "[fe80::1%en0]:22" {
		t.Errorf("dial addr = %q, want pass-through %q", got, "[fe80::1%en0]:22")
	}
}

func TestDialFiltered_FiltersSiteLocalIPv6(t *testing.T) {
	// fec0::/10 is deprecated by RFC 3879 but cheap to filter; some
	// misconfigured caches still emit it. Treat it like link-local.
	ips := []net.IPAddr{
		{IP: net.ParseIP("fec0::1")},
		{IP: net.ParseIP("192.168.1.1")},
	}
	d, attempts, mu := newCaptureDialer(t, ips, "192.168.1.1")
	conn, err := d.dialFiltered(context.Background(), "tcp", "claw1.local:22", d.cfg)
	if err != nil {
		t.Fatalf("dialFiltered: %v", err)
	}
	defer conn.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(*attempts) != 1 {
		t.Fatalf("expected 1 attempt, got %d: %+v", len(*attempts), *attempts)
	}
	if strings.Contains((*attempts)[0].addr, "fec0") {
		t.Errorf("dial addr = %q, must not include fec0:: site-local", (*attempts)[0].addr)
	}
}

func TestDialFiltered_HappyPathDualStack(t *testing.T) {
	// Regression: when the resolver returns a healthy mix (global IPv6
	// + IPv4), the dialer attempts candidates in resolver order until
	// one succeeds. No filtering should happen for either of these.
	ips := []net.IPAddr{
		{IP: net.ParseIP("2001:db8::1")},
		{IP: net.ParseIP("192.168.1.1")},
	}
	// Succeed on the first candidate so we know ordering is preserved
	// (no surprise re-shuffle in the filter).
	d, attempts, mu := newCaptureDialer(t, ips, "2001:db8::1")
	conn, err := d.dialFiltered(context.Background(), "tcp", "claw1.local:22", d.cfg)
	if err != nil {
		t.Fatalf("dialFiltered: %v", err)
	}
	defer conn.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(*attempts) != 1 {
		t.Fatalf("expected 1 attempt for happy path, got %d: %+v", len(*attempts), *attempts)
	}
	if !strings.Contains((*attempts)[0].addr, "2001:db8::1") {
		t.Errorf("dial addr = %q, want it to contain 2001:db8::1 (first resolver result)", (*attempts)[0].addr)
	}
}

func TestDialFiltered_FallbackAfterFirstFails(t *testing.T) {
	// When the first non-filtered candidate dial fails, the dialer
	// must continue down the list. This is also where the operational
	// recovery shows up for #42-like cases: even if the resolver still
	// returns a stale routable-but-dead IPv6 first, the IPv4 fallback
	// completes the dial.
	ips := []net.IPAddr{
		{IP: net.ParseIP("2001:db8::dead")},
		{IP: net.ParseIP("192.168.1.1")},
	}
	d, attempts, mu := newCaptureDialer(t, ips, "192.168.1.1")
	conn, err := d.dialFiltered(context.Background(), "tcp", "claw1.local:22", d.cfg)
	if err != nil {
		t.Fatalf("dialFiltered: %v", err)
	}
	defer conn.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(*attempts) != 2 {
		t.Fatalf("expected 2 attempts (first fails, second succeeds), got %d: %+v", len(*attempts), *attempts)
	}
	if !strings.Contains((*attempts)[1].addr, "192.168.1.1") {
		t.Errorf("second dial addr = %q, want it to contain 192.168.1.1", (*attempts)[1].addr)
	}
}

func TestDialFiltered_LiteralIPBypassesResolver(t *testing.T) {
	// If the operator passes a literal IP (even a link-local one)
	// we trust them and pass through. The whole filter is about
	// resolver staleness, not about second-guessing explicit input.
	d := NewDialer(WithAgentSocket(""))
	var attempted string
	d.resolver = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		t.Fatalf("resolver must not be called for literal IP; host=%s", host)
		return nil, nil
	}
	d.dialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		attempted = addr
		c1, c2 := net.Pipe()
		_ = c2.Close()
		return c1, nil
	}
	if _, err := d.dialFiltered(context.Background(), "tcp", "192.168.1.1:22", d.cfg); err != nil {
		t.Fatalf("literal IPv4 dialFiltered: %v", err)
	}
	if attempted != "192.168.1.1:22" {
		t.Errorf("literal IPv4 should pass through; got %q", attempted)
	}
}

func TestIsFilteredIP_Coverage(t *testing.T) {
	// Spot-check the predicate so a future refactor of the rule set
	// has an obvious place to fail loud.
	cases := []struct {
		ip       string
		filtered bool
	}{
		{"fe80::1", true},     // IPv6 link-local unicast
		{"fec0::1", true},     // IPv6 site-local (deprecated, still filtered)
		{"fd09::1", false},    // IPv6 unique-local — the ULA workers use
		{"2001:db8::1", false}, // IPv6 global
		{"::1", false},         // IPv6 loopback
		{"169.254.1.1", true},  // IPv4 link-local (APIPA)
		{"192.168.1.1", false}, // IPv4 private
		{"127.0.0.1", false},   // IPv4 loopback
	}
	for _, tc := range cases {
		got := isFilteredIP(net.ParseIP(tc.ip))
		if got != tc.filtered {
			t.Errorf("isFilteredIP(%s) = %v, want %v", tc.ip, got, tc.filtered)
		}
	}
}
