package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

func newHTTPClient(proxy, caFile string, insecureSkipVerify bool) (*http.Client, error) {
	transport, err := newHTTPTransport(proxy, caFile, insecureSkipVerify)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: transport}, nil
}

func newHTTPTransport(proxy, caFile string, insecureSkipVerify bool) (*http.Transport, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("default transport is %T, want *http.Transport", http.DefaultTransport)
	}
	transport := base.Clone()
	switch mode := strings.TrimSpace(proxy); {
	case mode == "":
	case isDirectProxyMode(mode):
		transport.Proxy = nil
	default:
		u, err := url.Parse(mode)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL: %w", err)
		}
		if strings.TrimSpace(u.Scheme) == "" || strings.TrimSpace(u.Host) == "" {
			return nil, fmt.Errorf("invalid proxy URL %q", proxy)
		}
		transport.Proxy = http.ProxyURL(u)
	}
	tlsConfig, err := newTLSConfig(caFile, insecureSkipVerify)
	if err != nil {
		return nil, err
	}
	if tlsConfig != nil {
		transport.TLSClientConfig = tlsConfig
	}
	return transport, nil
}

func validateProviderTransportConfig(prov *ProviderConfig) error {
	if prov == nil {
		return nil
	}
	_, err := newHTTPTransport(prov.Proxy, prov.CAFile, prov.Insecure)
	return err
}

func isDirectProxyMode(proxy string) bool {
	switch strings.TrimSpace(strings.ToLower(proxy)) {
	case "direct", "none", "off":
		return true
	default:
		return false
	}
}

func describeProxyMode(proxy string) string {
	mode := strings.TrimSpace(proxy)
	switch {
	case mode == "":
		return "env"
	case isDirectProxyMode(mode):
		return "direct"
	default:
		return mode
	}
}

func describeTLSMode(caFile string, insecureSkipVerify bool) string {
	switch {
	case insecureSkipVerify:
		return "insecure-skip-verify"
	case strings.TrimSpace(caFile) != "":
		return strings.TrimSpace(caFile)
	default:
		return "system"
	}
}

func newTLSConfig(caFile string, insecureSkipVerify bool) (*tls.Config, error) {
	caFile = strings.TrimSpace(caFile)
	if caFile == "" && !insecureSkipVerify {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecureSkipVerify}
	if caFile == "" {
		return cfg, nil
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	pemData, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read ca-file %q: %w", caFile, err)
	}
	if ok := roots.AppendCertsFromPEM(pemData); !ok {
		return nil, fmt.Errorf("parse ca-file %q: no certificates found", caFile)
	}
	cfg.RootCAs = roots
	return cfg, nil
}
