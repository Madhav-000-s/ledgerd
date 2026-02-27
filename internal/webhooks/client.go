package webhooks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SSRF errors.
var (
	ErrInsecureScheme = errors.New("webhooks: endpoint URL must use https")
	ErrPrivateAddress = errors.New("webhooks: endpoint resolves to a private or reserved address")
	ErrInvalidURL     = errors.New("webhooks: endpoint URL is not usable")
	ErrRedirect       = errors.New("webhooks: endpoint returned a redirect")
)

// blockedNetworks are the ranges an endpoint must never resolve to.
//
// The delivery worker POSTs to a URL the merchant chose, which makes it a confused
// deputy by default: without this it will happily fetch a cloud metadata endpoint or an
// internal admin service and hand the merchant whatever comes back.
var blockedNetworks = func() []*net.IPNet {
	cidrs := []string{
		"0.0.0.0/8",          // this host
		"10.0.0.0/8",         // RFC1918
		"100.64.0.0/10",      // CGNAT
		"127.0.0.0/8",        // loopback
		"169.254.0.0/16",     // link-local, and the cloud metadata service
		"172.16.0.0/12",      // RFC1918
		"192.0.0.0/24",       // IETF protocol assignments
		"192.0.2.0/24",       // TEST-NET-1
		"192.168.0.0/16",     // RFC1918
		"198.18.0.0/15",      // benchmarking
		"198.51.100.0/24",    // TEST-NET-2
		"203.0.113.0/24",     // TEST-NET-3
		"224.0.0.0/4",        // multicast
		"240.0.0.0/4",        // reserved
		"255.255.255.255/32", // broadcast
		"::1/128",            // loopback
		"fc00::/7",           // RFC4193 unique local
		"fe80::/10",          // link-local
		"ff00::/8",           // multicast
		"::/128",             // unspecified
		"64:ff9b::/96",       // NAT64, which can be used to reach IPv4 private space
	}

	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, network, err := net.ParseCIDR(c)
		if err != nil {
			panic("webhooks: bad built-in CIDR " + c)
		}
		out = append(out, network)
	}
	return out
}()

// IsBlockedIP reports whether an address is in a range an endpoint must not reach.
func IsBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() {
		return true
	}

	// An IPv4-mapped IPv6 address must be checked as the IPv4 address it is, or
	// ::ffff:127.0.0.1 walks straight through the v4 rules.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}

	for _, network := range blockedNetworks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientOptions configures the delivery client.
type ClientOptions struct {
	// AllowInsecure permits plain http. Local development only.
	AllowInsecure bool
	// AllowPrivateIPs disables the address guard. Local development only; config
	// validation refuses to start with it set in production.
	AllowPrivateIPs bool

	RequestTimeout time.Duration
	ConnectTimeout time.Duration
	// MaxResponseBytes bounds what is read back. The body is only used for diagnostics,
	// so an endpoint answering with a gigabyte must not be able to exhaust the worker.
	MaxResponseBytes int64

	// Resolver is replaceable so the DNS-rebinding guard can be tested with a stub.
	Resolver func(ctx context.Context, host string) ([]net.IPAddr, error)
}

// Client POSTs a signed payload to a merchant endpoint.
type Client struct {
	http *http.Client
	opts ClientOptions
}

// NewClient builds a delivery client whose dialer enforces the address rules.
func NewClient(opts ClientOptions) *Client {
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = 5 * time.Second
	}
	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = 2 * time.Second
	}
	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = 64 << 10
	}
	if opts.Resolver == nil {
		opts.Resolver = func(ctx context.Context, host string) ([]net.IPAddr, error) {
			return net.DefaultResolver.LookupIPAddr(ctx, host)
		}
	}

	c := &Client{opts: opts}

	transport := &http.Transport{
		DialContext:           c.dial,
		TLSHandshakeTimeout:   opts.ConnectTimeout,
		ResponseHeaderTimeout: opts.RequestTimeout,
		DisableKeepAlives:     false,
		MaxIdleConnsPerHost:   4,
		// Never inherit HTTP_PROXY. A proxy would resolve and connect on our behalf,
		// which puts the address check somewhere we do not control.
		Proxy: nil,
	}

	c.http = &http.Client{
		Transport: transport,
		Timeout:   opts.RequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// A redirect is the simplest way around a URL that was validated at
			// registration: the endpoint answers 302 and points at 169.254.169.254.
			return ErrRedirect
		},
	}
	return c
}

// dial resolves the host itself and rejects any blocked address before connecting.
//
// The check is at dial time, on the address actually being connected to, rather than at
// registration. Validating the URL when the endpoint is created leaves the DNS-rebinding
// hole wide open: a name that resolved to a public address then can resolve to
// 127.0.0.1 now, and nothing about the stored URL has changed.
func (c *Client) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidURL, address)
	}

	// A literal address needs no resolution, but still needs checking.
	if ip := net.ParseIP(host); ip != nil {
		if !c.opts.AllowPrivateIPs && IsBlockedIP(ip) {
			return nil, fmt.Errorf("%w: %s", ErrPrivateAddress, ip)
		}
		return c.dialer().DialContext(ctx, network, address)
	}

	resolved, err := c.opts.Resolver(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("webhooks: resolving %s: %w", host, err)
	}
	if len(resolved) == 0 {
		return nil, fmt.Errorf("%w: %s resolved to nothing", ErrInvalidURL, host)
	}

	// Connect to a checked address rather than re-resolving by name, so there is no gap
	// between the check and the connection for the answer to change in.
	var lastErr error
	for _, addr := range resolved {
		if !c.opts.AllowPrivateIPs && IsBlockedIP(addr.IP) {
			lastErr = fmt.Errorf("%w: %s resolves to %s", ErrPrivateAddress, host, addr.IP)
			continue
		}

		conn, err := c.dialer().DialContext(ctx, network, net.JoinHostPort(addr.IP.String(), port))
		if err != nil {
			lastErr = err
			continue
		}
		return conn, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("%w: %s", ErrPrivateAddress, host)
	}
	return nil, lastErr
}

func (c *Client) dialer() *net.Dialer {
	return &net.Dialer{Timeout: c.opts.ConnectTimeout}
}

// ValidateURL checks an endpoint URL at registration.
//
// This is the cheap early rejection, not the security boundary — that is the dial-time
// check, which is the only one a rebinding attack cannot walk around. Both exist because
// telling a merchant their URL is unusable when they save it is far better than silently
// failing every delivery.
func (c *Client) ValidateURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidURL, err)
	}

	switch parsed.Scheme {
	case "https":
	case "http":
		if !c.opts.AllowInsecure {
			return fmt.Errorf("%w: %s", ErrInsecureScheme, raw)
		}
	default:
		return fmt.Errorf("%w: scheme %q", ErrInvalidURL, parsed.Scheme)
	}

	if parsed.Host == "" {
		return fmt.Errorf("%w: no host", ErrInvalidURL)
	}
	if parsed.User != nil {
		return fmt.Errorf("%w: credentials in the URL", ErrInvalidURL)
	}

	host := parsed.Hostname()
	if ip := net.ParseIP(host); ip != nil && !c.opts.AllowPrivateIPs && IsBlockedIP(ip) {
		return fmt.Errorf("%w: %s", ErrPrivateAddress, ip)
	}
	return nil
}

// Response is the outcome of one delivery attempt.
type Response struct {
	StatusCode int
	Duration   time.Duration
	Body       string
	Err        error
}

// Succeeded reports whether the endpoint accepted the delivery.
func (r Response) Succeeded() bool {
	return r.Err == nil && r.StatusCode >= 200 && r.StatusCode < 300
}

// Deliver POSTs a signed payload.
//
// The body passed here is the exact byte slice that was signed. It is never
// re-serialized between signing and sending: a different key order or spacing would
// produce a payload the consumer's verifier rejects, for reasons that look like a bug on
// their side.
func (c *Client) Deliver(ctx context.Context, targetURL string, body []byte, headers map[string]string) Response {
	started := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, strings.NewReader(string(body)))
	if err != nil {
		return Response{Duration: time.Since(started), Err: fmt.Errorf("%w: %s", ErrInvalidURL, err)}
	}

	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", "ledgerd-webhooks/1")
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// A refused redirect is reported as its own error rather than a transport
		// failure, so the operator can tell an SSRF attempt from an outage.
		if errors.Is(err, ErrRedirect) {
			return Response{Duration: time.Since(started), Err: ErrRedirect}
		}
		return Response{Duration: time.Since(started), Err: err}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, c.opts.MaxResponseBytes))
		_ = resp.Body.Close()
	}()

	// Read a bounded prefix for diagnostics and discard the rest.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, c.opts.MaxResponseBytes))

	return Response{
		StatusCode: resp.StatusCode,
		Duration:   time.Since(started),
		Body:       string(snippet),
	}
}
