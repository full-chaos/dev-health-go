package clickhouse

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"os"
)

const maximumCABundleBytes = 1 << 20

// TLSConfigFromCABundle loads an optional ClickHouse CA bundle from disk
// into a *tls.Config. An empty path is not an error: it means "use the
// system trust store," matching every ACR ClickHouse caller's default.
// Shared by internal/runtime/hosted and cmd/acr-projector so this
// security-sensitive loading path has exactly one implementation.
func TLSConfigFromCABundle(path string) (configuration *tls.Config, resultErr error) {
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("ClickHouse CA bundle is invalid")
	}
	defer func() {
		if err := file.Close(); err != nil {
			configuration = nil
			resultErr = errors.Join(resultErr, errors.New("ClickHouse CA bundle is invalid"))
		}
	}()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maximumCABundleBytes {
		return nil, errors.New("ClickHouse CA bundle is invalid")
	}
	contents, err := io.ReadAll(io.LimitReader(file, maximumCABundleBytes+1))
	if err != nil || len(contents) > maximumCABundleBytes {
		return nil, errors.New("ClickHouse CA bundle is invalid")
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(contents) {
		return nil, errors.New("ClickHouse CA bundle is invalid")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, nil
}
