package e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GenerateSelfSignedCert generates a self-signed TLS certificate and key
// Returns the certificate and key as PEM-encoded bytes
// For test purposes, this generates a CA certificate and a server certificate signed by it
// The CA certificate is returned as the first value (for use in validation contexts)
// The server certificate is returned as the second value (for use as the server cert)
func GenerateSelfSignedCert(commonName string) (caCertPEM []byte, serverCertPEM []byte, serverKeyPEM []byte, err error) {
	// Generate CA private key
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to generate CA private key: %w", err)
	}

	// Create CA certificate template
	caTemplate := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: fmt.Sprintf("%s CA", commonName),
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	// Create CA certificate (self-signed)
	caCertDER, err := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create CA certificate: %w", err)
	}

	caCertPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: caCertDER,
	})

	// Generate server private key
	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to generate server private key: %w", err)
	}

	// Parse CA certificate for signing
	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to parse CA certificate: %w", err)
	}

	// Create server certificate template
	serverTemplate := x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			CommonName: commonName,
		},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{commonName, "localhost", "127.0.0.1"},
		IsCA:         false,
	}

	// Create server certificate signed by CA
	serverCertDER, err := x509.CreateCertificate(rand.Reader, &serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create server certificate: %w", err)
	}

	serverCertPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: serverCertDER,
	})

	serverKeyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(serverKey),
	})

	return caCertPEM, serverCertPEM, serverKeyPEM, nil
}

// CreateTLSSecret creates a Kubernetes secret with TLS certificate and key
func CreateTLSSecret(ctx context.Context, k8sClient *K8sClient, namespace, secretName, commonName string) error {
	_, serverCertPEM, serverKeyPEM, err := GenerateSelfSignedCert(commonName)
	if err != nil {
		return fmt.Errorf("failed to generate certificate: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": serverCertPEM,
			"tls.key": serverKeyPEM,
		},
	}

	_, err = k8sClient.GetClientset().CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			// Update existing secret
			_, err = k8sClient.GetClientset().CoreV1().Secrets(namespace).Update(ctx, secret, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("failed to update TLS secret: %w", err)
			}
		} else {
			return fmt.Errorf("failed to create TLS secret: %w", err)
		}
	}

	return nil
}

// CreateTLSConfig creates a tls.Config for client connections with the given certificate
func CreateTLSConfig(certPEM []byte) (*tls.Config, error) {
	// Parse the certificate to extract the CN or DNS names
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse certificate: %w", err)
	}

	// Create certificate pool and add the certificate
	certPool := x509.NewCertPool()
	if !certPool.AppendCertsFromPEM(certPEM) {
		return nil, fmt.Errorf("failed to parse certificate")
	}

	// Determine server name - prefer first DNS name, fallback to CN
	serverName := "localhost"
	if len(cert.DNSNames) > 0 {
		serverName = cert.DNSNames[0]
	} else if cert.Subject.CommonName != "" {
		serverName = cert.Subject.CommonName
	}

	return &tls.Config{
		RootCAs:    certPool,
		ServerName: serverName,
	}, nil
}
