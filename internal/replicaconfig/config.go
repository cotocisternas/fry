// Package replicaconfig loads explicit private replica trust and durable paths.
package replicaconfig

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

type TLS struct {
	CAFile          string `json:"ca_file"`
	CertificateFile string `json:"certificate_file"`
	KeyFile         string `json:"key_file"`
}

func (c TLS) Load() (*tls.Config, error) {
	ca, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, fmt.Errorf("replica CA file: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("replica CA file contains no certificates")
	}
	certificate, err := tls.LoadX509KeyPair(c.CertificateFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("replica certificate: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		RootCAs:      roots,
		ClientCAs:    roots,
		Certificates: []tls.Certificate{certificate},
	}, nil
}

func Key(raw string) ([32]byte, error) {
	var key [32]byte
	b, err := hex.DecodeString(raw)
	if err != nil || len(b) != len(key) {
		return key, fmt.Errorf("replica public-key fingerprint must be 64 hexadecimal characters")
	}
	copy(key[:], b)
	if key == ([32]byte{}) {
		return key, fmt.Errorf("zero replica fingerprint is invalid")
	}
	return key, nil
}

// Directory establishes the parent durability required by hotstore.Open.
func Directory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(absolute, 0o700); err != nil {
		return "", err
	}
	for p := absolute; ; p = filepath.Dir(p) {
		// G304: operator-configured directory ancestors are opened read-only for fsync; owner FRY
		// maintainers, review by 2027-01-18.
		//nolint:gosec // Validated configuration/fixture path.
		dir, e := os.Open(p)
		if e != nil {
			return "", e
		}
		e = dir.Sync()
		closeErr := dir.Close()
		if e != nil {
			return "", e
		}
		if closeErr != nil {
			return "", closeErr
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	return absolute, nil
}
