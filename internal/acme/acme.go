package acme

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

type Manager struct {
	manager *autocert.Manager
	domains []string
	email   string
	mu      sync.RWMutex
	cert    *tls.Certificate
}

func NewManager(email string, domains []string, cacheDir string) *Manager {
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(domains...),
		Cache:      autocert.DirCache(cacheDir),
		Email:      email,
	}
	return &Manager{
		manager: m,
		domains: domains,
		email:   email,
	}
}

func (m *Manager) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.RLock()
	if m.cert != nil && time.Until(m.cert.Leaf.NotAfter) > 24*time.Hour {
		cert := m.cert
		m.mu.RUnlock()
		return cert, nil
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cert != nil && time.Until(m.cert.Leaf.NotAfter) > 24*time.Hour {
		return m.cert, nil
	}

	cert, err := m.manager.GetCertificate(hello)
	if err != nil {
		return nil, fmt.Errorf("acme: get certificate: %w", err)
	}
	m.cert = cert
	return cert, nil
}

func (m *Manager) TLSConfig() *tls.Config {
	return &tls.Config{
		GetCertificate: m.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	}
}

func (m *Manager) Start(ctx context.Context) error {
	for _, domain := range m.domains {
		_, err := m.manager.GetCertificate(&tls.ClientHelloInfo{ServerName: domain})
		if err != nil {
			return fmt.Errorf("acme: pre-fetch %s: %w", domain, err)
		}
	}
	go m.renewLoop(ctx)
	return nil
}

func (m *Manager) renewLoop(ctx context.Context) {
	ticker := time.NewTicker(12 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, domain := range m.domains {
				_, err := m.manager.GetCertificate(&tls.ClientHelloInfo{ServerName: domain})
				if err != nil {
					continue
				}
			}
		}
	}
}