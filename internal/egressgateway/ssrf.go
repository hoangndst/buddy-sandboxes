package egressgateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

var (
	ErrRawIPNotAllowed = errors.New("direct IP literals are forbidden")
	cgnatCIDR          *net.IPNet
)

func init() {
	_, cgnatCIDR, _ = net.ParseCIDR("100.64.0.0/10")
}

// ValidatePublicIP verifies that an IP address is a routable public address,
// rejecting loopback, private RFC 1918 subnets, cloud metadata, carrier-grade NAT,
// and unspecified or broadcast addresses.
func ValidatePublicIP(ip net.IP) error {
	if ip == nil {
		return errors.New("nil IP address")
	}

	if ip.IsUnspecified() {
		return fmt.Errorf("unspecified IP forbidden: %s", ip)
	}

	ip4 := ip.To4()
	if ip4 != nil && ip4[0] == 0 {
		return fmt.Errorf("0.0.0.0/8 address block forbidden: %s", ip)
	}

	if ip.IsLoopback() {
		return fmt.Errorf("loopback IP forbidden: %s", ip)
	}

	if ip.IsPrivate() {
		return fmt.Errorf("private RFC 1918 IP forbidden: %s", ip)
	}

	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.Equal(net.ParseIP("169.254.169.254")) {
		return fmt.Errorf("link-local or cloud metadata IP forbidden: %s", ip)
	}

	if cgnatCIDR != nil && cgnatCIDR.Contains(ip) {
		return fmt.Errorf("carrier-grade NAT IP forbidden: %s", ip)
	}

	if ip.IsMulticast() || ip.Equal(net.ParseIP("255.255.255.255")) {
		return fmt.Errorf("multicast or broadcast IP forbidden: %s", ip)
	}

	return nil
}

// DialPinnedIP resolves a hostname to IPv4, validates all returned addresses against
// SSRF boundaries, and dials the first validated IP directly to prevent DNS rebinding.
func DialPinnedIP(ctx context.Context, host, port string, resolver *net.Resolver) (net.Conn, error) {
	if net.ParseIP(host) != nil {
		return nil, ErrRawIPNotAllowed
	}

	if resolver == nil {
		resolver = net.DefaultResolver
	}

	resolveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	ips, err := resolver.LookupIP(resolveCtx, "ip4", host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("dns resolution failed for %s: %w", host, err)
	}

	for _, ip := range ips {
		if err := ValidatePublicIP(ip); err != nil {
			return nil, err
		}
	}

	targetIP := ips[0]
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return dialer.DialContext(ctx, "tcp", net.JoinHostPort(targetIP.String(), port))
}
