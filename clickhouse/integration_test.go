package clickhouse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestIntegrationClient_matches_exact_evidence_digest(t *testing.T) {
	client, _ := integrationClient(t)
	digest := sha256.Sum256([]byte("acr:v1:ci:fixture"))
	rows, err := client.Query(context.Background(), `SELECT evidence_ref_id FROM (SELECT 'acr:v1:ci:fixture' evidence_ref_id) WHERE lower(hex(SHA256(evidence_ref_id))) = {evidence_locator_hash:String} LIMIT 2`, []Binding{{Name: "evidence_locator_hash", Value: hex.EncodeToString(digest[:])}})
	if err != nil {
		t.Fatalf("query exact evidence digest: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("exact evidence digest returned no rows: %v", rows.Err())
	}
	var locator string
	if err := rows.Scan(&locator); err != nil || locator != "acr:v1:ci:fixture" {
		t.Fatalf("exact evidence digest locator = %q, error = %v", locator, err)
	}
	if rows.Next() || rows.Err() != nil {
		t.Fatalf("exact evidence digest returned multiple rows: %v", rows.Err())
	}
}

func TestIntegrationClient_queries_read_only_ClickHouse(t *testing.T) {
	// Given
	client, options := integrationClient(t)

	// When
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
	rows, err := client.Query(context.Background(), "SELECT 1", nil)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	defer rows.Close()

	// Then
	if !rows.Next() {
		t.Fatalf("SELECT 1 returned no rows: %v", rows.Err())
	}
	var value uint8
	if err := rows.Scan(&value); err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if value != 1 {
		t.Fatalf("SELECT 1 value = %d", value)
	}
	if _, err := client.Query(context.Background(), "INSERT INTO evidence VALUES ('forbidden')", nil); !errors.Is(err, ErrUnsafeStatement) {
		t.Fatalf("INSERT error = %v, want ErrUnsafeStatement", err)
	}
	if _, err := client.Query(context.Background(), "SELECT 1; SELECT 2", nil); !errors.Is(err, ErrUnsafeStatement) {
		t.Fatalf("multi-statement error = %v, want ErrUnsafeStatement", err)
	}
	assertIntegrationParameterEncoding(t, client)
	assertIntegrationExecutionLimit(t, options)
	assertIntegrationServerRejectsMutation(t, options)
}

func TestIntegrationClient_native_readonly_fixture_is_not_skipped(t *testing.T) {
	// Given
	client, options := integrationClient(t)

	// When
	err := client.Ping(context.Background())

	// Then
	if err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
	assertIntegrationServerRejectsMutation(t, options)
}

func assertIntegrationExecutionLimit(t *testing.T, options Options) {
	t.Helper()

	// Given
	limited, err := NewClickHouseQueryClientWithOptions(Options{DSN: options.DSN, TLS: options.TLS, MaxExecutionTime: 1, QueryTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("create execution-limited client: %v", err)
	}
	t.Cleanup(func() { _ = limited.Close() })

	// When
	rows, err := limited.Query(context.Background(), "SELECT getSetting('max_execution_time')", nil)
	if err != nil {
		t.Fatalf("query max_execution_time: %v", err)
	}
	t.Cleanup(func() { _ = rows.Close() })

	// Then
	if !rows.Next() {
		t.Fatalf("max_execution_time returned no rows: %v", rows.Err())
	}
	var limit float64
	if err := rows.Scan(&limit); err != nil || limit != 1 {
		t.Fatalf("max_execution_time = %v, %v; want 1, nil", limit, err)
	}
	started := time.Now()
	timeoutRows, err := limited.Query(context.Background(), "SELECT sleepEachRow(1) FROM numbers(3) SETTINGS max_block_size = 1", nil)
	if err == nil {
		for timeoutRows.Next() {
		}
		err = timeoutRows.Err()
		if closeErr := timeoutRows.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	if err == nil {
		t.Fatal("execution-limited query unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("execution-limited query took %v, want at most 2s", elapsed)
	}
}

// arrayStringEscapingCorpus is the CHAOS-4745 property-style escaping
// corpus: every element is chosen to stress a specific interaction between
// this package's clickHouseStringArray encoding and ClickHouse's
// native-protocol Array(String) parameter parser -- quotes, backslashes,
// the two in direct adjacency (both orders), runs of only one or the
// other, structural characters the array-literal grammar itself uses
// (comma, brackets), unicode, and the empty string.
var arrayStringEscapingCorpus = []string{
	"",
	"plain-value",
	"O'Brien's project",
	`back\slash`,
	`a\'b`,   // backslash directly followed by a quote
	`a'\b`,   // quote directly followed by a backslash
	`\'\'\'`, // alternating run
	"''''",   // run of only quotes
	`\\\\`,   // run of only backslashes
	"comma,separated,value",
	"[bracketed-value]",
	"a,'b\\c[d]",
	"héllo wörld 日本語 🚀",
	"tab\tand\nnewline",
}

// TestIntegrationClient_binds_Array_String_values_byte_exact is the
// CHAOS-4745 red/green proof: clickHouseStringArray's escaping must
// round-trip every element of arrayStringEscapingCorpus through ClickHouse's
// real native-protocol Array(String) query-parameter parser byte-exact, not
// merely avoid an error. On the pre-fix \' escaping, this fails outright
// (ClickHouse rejects the query -- "Cannot parse escape sequence") the
// moment the corpus includes an embedded quote.
func TestIntegrationClient_binds_Array_String_values_byte_exact(t *testing.T) {
	client, _ := integrationClient(t)

	rows, err := client.Query(context.Background(), "SELECT {values:Array(String)}", []Binding{{Name: "values", Value: arrayStringEscapingCorpus}})
	if err != nil {
		t.Fatalf("Query() error = %v (want the Array(String) binding to be accepted)", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("Array(String) round-trip returned no rows: %v", rows.Err())
	}
	var got []string
	if err := rows.Scan(&got); err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if !reflect.DeepEqual(got, arrayStringEscapingCorpus) {
		for i := range arrayStringEscapingCorpus {
			if i >= len(got) {
				t.Errorf("element %d missing from result (want %q)", i, arrayStringEscapingCorpus[i])
				continue
			}
			if got[i] != arrayStringEscapingCorpus[i] {
				t.Errorf("element %d = %q (%d bytes), want %q (%d bytes)", i, got[i], len(got[i]), arrayStringEscapingCorpus[i], len(arrayStringEscapingCorpus[i]))
			}
		}
		if len(got) != len(arrayStringEscapingCorpus) {
			t.Errorf("result has %d elements, want %d", len(got), len(arrayStringEscapingCorpus))
		}
		t.Fatalf("Array(String) round-trip mismatch:\n got  = %#v\n want = %#v", got, arrayStringEscapingCorpus)
	}
}

func assertIntegrationParameterEncoding(t *testing.T, client *Client) {
	t.Helper()

	// Given
	ctx := context.Background()

	// When
	nilRows, err := client.Query(ctx, "SELECT {as_of:Nullable(DateTime64(3, 'UTC'))} IS NULL", []Binding{{Name: "as_of", Value: nil}})
	if err != nil {
		t.Fatalf("query nil DateTime64: %v", err)
	}
	t.Cleanup(func() { _ = nilRows.Close() })
	timeRows, err := client.Query(ctx, "SELECT toString({as_of:Nullable(DateTime64(3, 'UTC'))})", []Binding{{Name: "as_of", Value: time.Date(2026, time.July, 10, 11, 0, 0, 123_456_789, time.UTC)}})
	if err != nil {
		t.Fatalf("query DateTime64: %v", err)
	}
	t.Cleanup(func() { _ = timeRows.Close() })

	// Then
	if !nilRows.Next() {
		t.Fatalf("nil DateTime64 returned no rows: %v", nilRows.Err())
	}
	var isNull uint8
	if err := nilRows.Scan(&isNull); err != nil || isNull != 1 {
		t.Fatalf("nil DateTime64 result = %d, %v; want 1, nil", isNull, err)
	}
	if !timeRows.Next() {
		t.Fatalf("DateTime64 returned no rows: %v", timeRows.Err())
	}
	var formatted string
	if err := timeRows.Scan(&formatted); err != nil || formatted != "2026-07-10 11:00:00.123" {
		t.Fatalf("DateTime64 result = %q, %v; want UTC milliseconds", formatted, err)
	}
}
