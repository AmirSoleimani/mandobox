package reconcile

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// LoadClientTLS builds a client TLS config presenting the given client certificate and
// trusting server certificates signed by serverCAFile (mTLS to mando-agent).
func LoadClientTLS(certFile, keyFile, serverCAFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load client keypair: %w", err)
	}
	caPEM, err := os.ReadFile(serverCAFile)
	if err != nil {
		return nil, fmt.Errorf("read server CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("server CA %s contains no certificates", serverCAFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
