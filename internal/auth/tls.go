package auth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

const ALPN = "mule-v1"

func TLSConfig(secret []byte, role Role) (*tls.Config, error) {
	peer := RoleExit
	if role == RoleExit {
		peer = RoleForward
	}

	caCert, caKey, err := caCertificate(secret)
	if err != nil {
		return nil, err
	}
	cert, err := roleCertificate(secret, role, caCert, caKey)
	if err != nil {
		return nil, err
	}
	expectedPeer, err := RolePublicKey(secret, peer)
	if err != nil {
		return nil, err
	}

	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{ALPN},
		Certificates: []tls.Certificate{cert},
		ServerName:   "mule-exit",
		RootCAs:      pool,
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("missing peer certificate")
			}
			pub, ok := cs.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
			if !ok {
				return fmt.Errorf("unexpected peer public key type")
			}
			if !bytes.Equal(pub, expectedPeer) {
				return fmt.Errorf("unexpected peer identity")
			}
			return nil
		},
	}
	return cfg, nil
}

func caCertificate(secret []byte) (*x509.Certificate, ed25519.PrivateKey, error) {
	key, err := derivePrivateKey(secret, "mule/v1/ca identity")
	if err != nil {
		return nil, nil, err
	}
	serial, err := serialNumber(secret, "mule/v1/ca serial")
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "mule v1 derived ca"},
		NotBefore:             time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(2124, 1, 1, 0, 0, 0, 0, time.UTC),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, nil, fmt.Errorf("create ca certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func roleCertificate(secret []byte, role Role, ca *x509.Certificate, caKey ed25519.PrivateKey) (tls.Certificate, error) {
	label, err := roleIdentityLabel(role)
	if err != nil {
		return tls.Certificate{}, err
	}
	key, err := derivePrivateKey(secret, label)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := serialNumber(secret, "mule/v1/"+string(role)+" certificate serial")
	if err != nil {
		return tls.Certificate{}, err
	}

	eku := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	dnsNames := []string{"mule-" + string(role)}
	if role == RoleExit {
		eku = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "mule-" + string(role)},
		DNSNames:              dnsNames,
		NotBefore:             time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(2124, 1, 1, 0, 0, 0, 0, time.UTC),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           eku,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, key.Public(), caKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create role certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, err
	}
	return cert, nil
}

func serialNumber(secret []byte, label string) (*big.Int, error) {
	b, err := deriveBytes(secret, label, 16)
	if err != nil {
		return nil, err
	}
	b[0] &= 0x7f
	if allZero(b) {
		b[len(b)-1] = 1
	}
	return new(big.Int).SetBytes(b), nil
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
