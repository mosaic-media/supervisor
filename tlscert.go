// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 the Mosaic authors
// Linking exception: see LICENSE-EXCEPTION.

package supervisor

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"
)

// selfSignedValidity is deliberately short. A self-signed certificate is a
// stopgap for a LAN install with no domain, and one that expires in a year
// would let an install sit on it indefinitely without anybody deciding to.
const selfSignedValidity = 90 * 24 * time.Hour

// TLSCertificate resolves the certificate the front door serves.
//
// An operator-supplied pair is loaded and used. Otherwise a self-signed one is
// generated *in memory* for this boot: nothing is written to disk, because a
// generated key that persists is a key somebody has to be told about, back up
// and rotate, and this one is not worth any of that. The cost is that clients
// see a new certificate after a restart, which is the correct signal — it says
// "this is not a real certificate" every time rather than once.
func TLSCertificate(cfg Config) (tls.Certificate, bool, error) {
	if cfg.TLSConfigured() {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return tls.Certificate{}, false, fmt.Errorf("loading the configured certificate: %w", err)
		}
		return cert, false, nil
	}
	cert, err := selfSigned()
	return cert, true, err
}

func selfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generating a key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generating a serial: %w", err)
	}

	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"Mosaic"}, CommonName: "mosaic-supervisor"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(selfSignedValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	template.DNSNames, template.IPAddresses = localNames()

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("creating the certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// localNames collects the names this host answers to, so the generated
// certificate covers the address a person actually types. A LAN install is
// reached by IP far more often than by name, and a certificate that covers
// only "localhost" fails on the one address that matters.
func localNames() ([]string, []net.IP) {
	names := []string{"localhost"}
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}

	if host, err := os.Hostname(); err == nil && host != "" && host != "localhost" {
		names = append(names, host)
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return names, ips
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.IsLinkLocalUnicast() {
			continue
		}
		ips = append(ips, ipNet.IP)
	}
	return names, ips
}
