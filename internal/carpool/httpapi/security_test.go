package httpapi

import (
	"crypto/tls"
	"net/http"
	"testing"
)

func TestOriginValidator(t *testing.T) {
	validator, errCreate := NewOriginValidator([]string{"https://admin.example.com/"})
	if errCreate != nil {
		t.Fatalf("NewOriginValidator() error = %v", errCreate)
	}
	tests := []struct {
		name    string
		origin  string
		host    string
		tls     bool
		allowed bool
	}{
		{name: "same http origin", origin: "http://proxy.example.com", host: "proxy.example.com", allowed: true},
		{name: "same https origin", origin: "https://proxy.example.com", host: "proxy.example.com", tls: true, allowed: true},
		{name: "configured origin", origin: "https://admin.example.com", host: "proxy.example.com", allowed: true},
		{name: "cross origin", origin: "https://evil.example.com", host: "proxy.example.com"},
		{name: "missing origin", host: "proxy.example.com"},
		{name: "null origin", origin: "null", host: "proxy.example.com"},
		{name: "origin path", origin: "https://proxy.example.com/path", host: "proxy.example.com", tls: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, errRequest := http.NewRequest(http.MethodPost, "http://"+test.host+"/carpool/api/v1/session", nil)
			if errRequest != nil {
				t.Fatalf("http.NewRequest() error = %v", errRequest)
			}
			request.Host = test.host
			request.Header.Set("Origin", test.origin)
			if test.tls {
				request.TLS = &tls.ConnectionState{}
			}
			if got := validator.Allows(request); got != test.allowed {
				t.Fatalf("Allows() = %t, want %t", got, test.allowed)
			}
		})
	}
}

func TestTrustedClientAddress(t *testing.T) {
	networks, errParse := ParseTrustedProxyCIDRs([]string{"10.0.0.0/8", "192.0.2.0/24"})
	if errParse != nil {
		t.Fatalf("ParseTrustedProxyCIDRs() error = %v", errParse)
	}
	tests := []struct {
		name       string
		remoteAddr string
		forwarded  string
		want       string
	}{
		{name: "untrusted peer ignores header", remoteAddr: "203.0.113.8:1234", forwarded: "198.51.100.1", want: "203.0.113.8"},
		{name: "trusted peer accepts client", remoteAddr: "10.0.0.2:1234", forwarded: "198.51.100.2", want: "198.51.100.2"},
		{name: "walks chain from right", remoteAddr: "10.0.0.2:1234", forwarded: "198.51.100.9, 203.0.113.7, 192.0.2.10", want: "203.0.113.7"},
		{name: "invalid header falls back", remoteAddr: "10.0.0.2:1234", forwarded: "not-an-ip", want: "10.0.0.2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, errRequest := http.NewRequest(http.MethodPost, "http://proxy.example.com", nil)
			if errRequest != nil {
				t.Fatalf("http.NewRequest() error = %v", errRequest)
			}
			request.RemoteAddr = test.remoteAddr
			request.Header.Set("X-Forwarded-For", test.forwarded)
			if got := TrustedClientAddress(request, networks); got != test.want {
				t.Fatalf("TrustedClientAddress() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCSRFTokenEqual(t *testing.T) {
	if !CSRFTokenEqual("csrf-value", "csrf-value") {
		t.Fatal("CSRFTokenEqual() rejected equal values")
	}
	if CSRFTokenEqual("csrf-value", "csrf-other") || CSRFTokenEqual("", "") {
		t.Fatal("CSRFTokenEqual() accepted invalid values")
	}
}
