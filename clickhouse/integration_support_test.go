package clickhouse

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	clickhousedriver "github.com/ClickHouse/clickhouse-go/v2"
)

func integrationClient(t *testing.T) (*Client, Options) {
	t.Helper()
	dsn := os.Getenv("ACR_CLICKHOUSE_INTEGRATION_DSN")
	if dsn == "" {
		if os.Getenv("ACR_CLICKHOUSE_INTEGRATION_REQUIRED") == "1" {
			t.Fatal("ACR_CLICKHOUSE_INTEGRATION_DSN is required when native ClickHouse integration is mandatory")
		}
		t.Skip("ACR_CLICKHOUSE_INTEGRATION_DSN is required for the real ClickHouse integration test")
	}
	if os.Getenv("ACR_CLICKHOUSE_INTEGRATION_ISOLATED") != "1" {
		if os.Getenv("ACR_CLICKHOUSE_INTEGRATION_REQUIRED") == "1" {
			t.Fatal("ACR_CLICKHOUSE_INTEGRATION_ISOLATED=1 is required when native ClickHouse integration is mandatory")
		}
		t.Skip("ACR_CLICKHOUSE_INTEGRATION_ISOLATED=1 is required before the integration test can target seeded data")
	}
	options := Options{DSN: dsn, TLS: integrationTLSConfig(t), QueryTimeout: 10 * time.Second}
	client, err := NewClickHouseQueryClientWithOptions(options)
	if err != nil {
		t.Fatalf("NewClickHouseQueryClientWithOptions() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Errorf("Close() error = %v", closeErr)
		}
	})
	return client, options
}

func integrationTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	certificatePath := os.Getenv("ACR_CLICKHOUSE_INTEGRATION_CA_FILE")
	if certificatePath == "" {
		return nil
	}
	certificate, err := os.ReadFile(certificatePath)
	if err != nil {
		t.Fatalf("read ClickHouse CA file: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certificate) {
		t.Fatal("parse ClickHouse CA file")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: os.Getenv("ACR_CLICKHOUSE_INTEGRATION_TLS_SERVER_NAME")}
}

func TestIntegrationTLSConfig_allows_plaintext_when_CA_file_unset(t *testing.T) {
	// Given
	t.Setenv("ACR_CLICKHOUSE_INTEGRATION_CA_FILE", "")

	// When
	config := integrationTLSConfig(t)

	// Then
	if config != nil {
		t.Fatalf("integrationTLSConfig() = %#v, want nil", config)
	}
}

func assertIntegrationServerRejectsMutation(t *testing.T, options Options) {
	t.Helper()

	// Given
	configured, err := clickhousedriver.ParseDSN(options.DSN)
	if err != nil {
		t.Fatalf("parse direct ClickHouse DSN: %v", err)
	}
	applyOptions(configured, options)
	connection, err := clickhousedriver.Open(configured)
	if err != nil {
		t.Fatalf("open direct ClickHouse connection: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	// When
	rows, err := connection.Query(context.Background(), "SELECT getSetting('readonly')")
	if err != nil {
		t.Fatalf("read enforced readonly setting: %v", err)
	}
	t.Cleanup(func() { _ = rows.Close() })
	if !rows.Next() {
		t.Fatalf("enforced readonly setting returned no rows: %v", rows.Err())
	}
	var readonly uint8
	if err := rows.Scan(&readonly); err != nil {
		t.Fatalf("scan enforced readonly setting: %v", err)
	}
	if readonly != 2 {
		t.Fatalf("getSetting('readonly') = %d, want server profile 2", readonly)
	}
	err = connection.Exec(context.Background(), "INSERT INTO ci_pipeline_runs (run_id, repo_id, branch, status, started_at, finished_at) VALUES ('forbidden', '00000000-0000-0000-0000-000000000001', 'main', 'failure', now64(3), now64(3))")

	// Then
	if err == nil {
		t.Fatal("server accepted INSERT for the configured read-only user")
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("server mutation error was cancellation: %v", err)
	}
	if !strings.Contains(strings.ToLower(fmt.Sprint(err)), "readonly") {
		t.Fatalf("server mutation error = %v, want readonly-specific denial", err)
	}
}
