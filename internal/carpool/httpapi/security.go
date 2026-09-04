package httpapi

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// OriginValidator validates browser origins against the request site and configured origins.
type OriginValidator struct {
	trusted map[string]struct{}
}

// NewOriginValidator validates and normalizes exact trusted origins.
func NewOriginValidator(origins []string) (OriginValidator, error) {
	validator := OriginValidator{trusted: make(map[string]struct{}, len(origins))}
	for _, rawOrigin := range origins {
		origin, errNormalize := normalizeOrigin(rawOrigin)
		if errNormalize != nil {
			return OriginValidator{}, errNormalize
		}
		validator.trusted[origin] = struct{}{}
	}
	return validator, nil
}

// Allows reports whether a request has a non-null trusted Origin header.
func (v OriginValidator) Allows(request *http.Request) bool {
	if request == nil {
		return false
	}
	origin, errOrigin := normalizeOrigin(request.Header.Get("Origin"))
	if errOrigin != nil {
		return false
	}
	if _, ok := v.trusted[origin]; ok {
		return true
	}
	return origin == requestOrigin(request)
}

// ParseTrustedProxyCIDRs parses configured proxy networks once at startup.
func ParseTrustedProxyCIDRs(values []string) ([]*net.IPNet, error) {
	networks := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, network, errParse := net.ParseCIDR(strings.TrimSpace(value))
		if errParse != nil {
			return nil, fmt.Errorf("carpool: parse trusted proxy CIDR: %w", errParse)
		}
		networks = append(networks, network)
	}
	return networks, nil
}

// TrustedClientAddress returns a forwarded client only when the direct peer is trusted.
func TrustedClientAddress(request *http.Request, trusted []*net.IPNet) string {
	if request == nil {
		return ""
	}
	peer := parseAddressIP(request.RemoteAddr)
	if peer == nil {
		return strings.TrimSpace(request.RemoteAddr)
	}
	if !ipInNetworks(peer, trusted) {
		return peer.String()
	}

	forwarded := parseForwardedChain(request.Header.Values("X-Forwarded-For"))
	chain := append(forwarded, peer)
	for index := len(chain) - 1; index >= 0; index-- {
		candidate := chain[index]
		if !ipInNetworks(candidate, trusted) {
			return candidate.String()
		}
	}
	return peer.String()
}

// CSRFTokenEqual compares fixed-format CSRF values without data-dependent early exit.
func CSRFTokenEqual(expected, actual string) bool {
	return expected != "" && len(expected) == len(actual) && subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) == 1
}

func normalizeOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "null") {
		return "", fmt.Errorf("carpool: origin is required")
	}
	parsed, errParse := url.Parse(raw)
	if errParse != nil || parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", fmt.Errorf("carpool: invalid origin")
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host), nil
}

func requestOrigin(request *http.Request) string {
	scheme := "http"
	if request.TLS != nil {
		scheme = "https"
	}
	host := strings.ToLower(strings.TrimSpace(request.Host))
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}

func parseAddressIP(address string) net.IP {
	address = strings.TrimSpace(address)
	if host, _, errSplit := net.SplitHostPort(address); errSplit == nil {
		address = host
	}
	return net.ParseIP(strings.Trim(address, "[]"))
}

func parseForwardedChain(values []string) []net.IP {
	var chain []net.IP
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			if ip := parseAddressIP(part); ip != nil {
				chain = append(chain, ip)
			}
		}
	}
	return chain
}

func ipInNetworks(ip net.IP, networks []*net.IPNet) bool {
	for _, network := range networks {
		if network != nil && network.Contains(ip) {
			return true
		}
	}
	return false
}
