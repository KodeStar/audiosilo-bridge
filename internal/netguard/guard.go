// Package netguard fetches feeds and artwork through an address-checked dialer.
package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

var ErrBlocked = errors.New("the feed address is not public or explicitly allowed")

var notPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("64:ff9b:1::/48"), netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("::/96"), netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("fec0::/10"),
}
var nat64 = netip.MustParsePrefix("64:ff9b::/96")

type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type Options struct {
	AllowNetworks []netip.Prefix
	Resolver      Resolver
	Version       string
}

type Client struct {
	transport *http.Transport
	resolver  Resolver
	allow     []netip.Prefix
	version   string
	stall     time.Duration
	wait      func(context.Context, time.Duration) error
}

func New(opts Options) *Client {
	if opts.Resolver == nil {
		opts.Resolver = net.DefaultResolver
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	c := &Client{resolver: opts.Resolver, allow: append([]netip.Prefix(nil), opts.AllowNetworks...), version: opts.Version, stall: 30 * time.Second, wait: wait}
	c.transport = &http.Transport{Proxy: nil, DialContext: c.dial, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second, DisableCompression: true, MaxIdleConns: 10, IdleConnTimeout: 30 * time.Second}
	return c
}

func (c *Client) Close() { c.transport.CloseIdleConnections() }

func (c *Client) allowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, p := range c.allow {
		if p.Contains(ip) {
			return true
		}
	}
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range notPublic {
		if p.Contains(ip) {
			return false
		}
	}
	if nat64.Contains(ip) {
		a := ip.As16()
		return c.allowed(netip.AddrFrom4([4]byte{a[12], a[13], a[14], a[15]}))
	}
	return true
}

// control runs after resolution, immediately before the socket connects.
func (c *Client) control(_, address string, _ syscall.RawConn) error {
	a, err := netip.ParseAddrPort(address)
	if err != nil || !c.allowed(a.Addr()) {
		return ErrBlocked
	}
	return nil
}

func (c *Client) dial(ctx context.Context, network, address string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, ErrBlocked
	}
	ips, err := c.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Code: "dns", Message: "The feed host could not be resolved."}
	}
	if len(ips) == 0 {
		return nil, &Error{Code: "dns", Message: "The feed host has no address."}
	}
	// Check the entire answer, then dial those exact addresses: no second DNS lookup.
	for _, ip := range ips {
		if !c.allowed(ip) {
			return nil, ErrBlocked
		}
	}
	d := &net.Dialer{Timeout: 10 * time.Second, Control: c.control}
	for _, ip := range ips {
		conn, e := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if e == nil {
			return conn, nil
		}
		err = e
	}
	return nil, err
}

func validURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Hostname() != "" && (u.Scheme == "http" || u.Scheme == "https")
}
