package clickhouse

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"testing"
	"time"

	clickhousedriver "github.com/ClickHouse/clickhouse-go/v2"
)

func TestNewClickHouseQueryClientWithOptions_allows_plaintext_DSN_in_all_environments(t *testing.T) {
	for _, environment := range []string{"", "staging", "production"} {
		t.Run(environment, func(t *testing.T) {
			// Given
			options := Options{DSN: "clickhouse://readonly@example.invalid:9000/default"}

			// When
			client, err := NewClickHouseQueryClientWithOptions(options)

			// Then
			if err != nil {
				t.Fatalf("NewClickHouseQueryClientWithOptions() error = %v, want nil", err)
			}
			if err := client.Close(); err != nil {
				t.Fatalf("Close() error = %v, want nil", err)
			}
		})
	}
}

func TestNewClickHouseQueryClientWithOptions_allows_verified_TLS_in_production(t *testing.T) {
	// Given
	options := Options{
		DSN: "https://readonly@example.invalid:8443/default?secure=true",
	}

	// When
	client, err := NewClickHouseQueryClientWithOptions(options)

	// Then
	if err != nil {
		t.Fatalf("NewClickHouseQueryClientWithOptions() error = %v, want nil", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
}

func TestNewClickHouseQueryClientWithOptions_rejects_plain_HTTP_with_TLS_config(t *testing.T) {
	// Given
	options := Options{
		DSN: "http://readonly@example.invalid:8123/default",
		TLS: &tls.Config{MinVersion: tls.VersionTLS12},
	}

	// When
	_, err := NewClickHouseQueryClientWithOptions(options)

	// Then
	if !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("NewClickHouseQueryClientWithOptions() error = %v, want ErrInvalidConfiguration", err)
	}
}

func TestNewClickHouseQueryClientWithOptions_rejects_TLS_without_verification(t *testing.T) {
	// Given
	dsn := "clickhouse://readonly@example.invalid:9440/default?secure=true&skip_verify=true"

	// When
	_, err := NewClickHouseQueryClientWithOptions(Options{DSN: dsn})

	// Then
	if !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("NewClickHouseQueryClientWithOptions() error = %v, want ErrInvalidConfiguration", err)
	}
}

func TestNewClickHouseQueryClientWithOptions_rejects_client_readonly_setting(t *testing.T) {
	for _, readonly := range []string{"0", "1", "2"} {
		t.Run("readonly="+readonly, func(t *testing.T) {
			// Given
			dsn := "clickhouse://readonly@example.invalid:9440/default?secure=true&skip_verify=false&readonly=" + readonly

			// When
			_, err := NewClickHouseQueryClientWithOptions(Options{DSN: dsn, TLS: &tls.Config{MinVersion: tls.VersionTLS12}})

			// Then
			if !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("NewClickHouseQueryClientWithOptions() error = %v, want ErrInvalidConfiguration", err)
			}
		})
	}
}

func TestApplyOptions_applies_default_execution_limits(t *testing.T) {
	// Given
	configured := &clickhousedriver.Options{}

	// When
	applyOptions(configured, Options{})

	// Then
	if _, exists := configured.Settings["readonly"]; exists || configured.Settings["max_execution_time"] != uint(10) || configured.Settings["max_result_rows"] != uint(1_000) || configured.Settings["max_bytes_to_read"] != uint64(64<<20) {
		t.Fatalf("query settings = %#v, want bounded limits without a client read-only override", configured.Settings)
	}
}

// TestApplyOptions_appliesConfiguredMaxBytesToRead pins CHAOS-3848's config
// plumbing at the layer that would have silently swallowed it: a caller that
// sets Options.MaxBytesToRead must see the EXACT configured value reach the
// driver's Settings, not just "some default". Before CHAOS-3848,
// Options.MaxBytesToRead existed on the struct but nothing outside this
// package (and its tests) ever set it, so this override path had no
// production caller -- this test would still have passed on the old code
// (the field was always plumbed inside applyOptions), which is why part 1's
// real closure test lives in internal/config, pinning that the CONFIGURED
// value actually reaches this struct in the first place.
func TestApplyOptions_appliesConfiguredMaxBytesToRead(t *testing.T) {
	// Given
	configured := &clickhousedriver.Options{}
	limit := uint64(64 << 20)

	// When
	applyOptions(configured, Options{MaxBytesToRead: &limit})

	// Then
	if configured.Settings["max_bytes_to_read"] != uint64(64<<20) {
		t.Fatalf("max_bytes_to_read = %v, want %d", configured.Settings["max_bytes_to_read"], uint64(64<<20))
	}
}

func TestApplyOptions_preserves_DSN_settings_and_TLS_server_name(t *testing.T) {
	// Given
	dsnRoots := x509.NewCertPool()
	runtimeRoots := x509.NewCertPool()
	callerSettings := clickhousedriver.Settings{
		"custom_setting":     "preserve",
		"max_execution_time": uint(999),
	}
	configured := &clickhousedriver.Options{
		TLS:             &tls.Config{ServerName: "clickhouse.internal", RootCAs: dsnRoots},
		DialTimeout:     3 * time.Second,
		ReadTimeout:     4 * time.Second,
		MaxOpenConns:    5,
		MaxIdleConns:    2,
		ConnMaxLifetime: 6 * time.Minute,
		Settings:        callerSettings,
	}

	// When
	applyOptions(configured, Options{TLS: &tls.Config{RootCAs: runtimeRoots}})

	// Then
	if configured.TLS.ServerName != "clickhouse.internal" || configured.TLS.RootCAs != runtimeRoots {
		t.Fatalf("TLS = %#v, want preserved DSN server name and runtime roots", configured.TLS)
	}
	if configured.DialTimeout != 3*time.Second || configured.ReadTimeout != 4*time.Second || configured.MaxOpenConns != 5 || configured.MaxIdleConns != 2 || configured.ConnMaxLifetime != 6*time.Minute {
		t.Fatalf("DSN connection settings were overwritten: %#v", configured)
	}
	if configured.Settings["custom_setting"] != "preserve" || configured.Settings["max_execution_time"] != uint(10) {
		t.Fatalf("settings = %#v, want preserved caller settings plus bounded query limits", configured.Settings)
	}
	if _, exists := callerSettings["readonly"]; exists || callerSettings["max_execution_time"] != uint(999) {
		t.Fatalf("caller settings were mutated: %#v", callerSettings)
	}
}
