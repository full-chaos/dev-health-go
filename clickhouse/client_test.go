package clickhouse

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestClient_Query_translates_named_bindings_without_interpolation(t *testing.T) {
	// Given
	connection := &fakeConnection{}
	client := newClient(connection, time.Second)
	statement := "SELECT * FROM evidence WHERE org_id = {org_id:String}"
	bindings := []Binding{{Name: "org_id", Value: "org-1'; DROP TABLE evidence"}}

	// When
	_, err := client.Query(context.Background(), statement, bindings)

	// Then
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if connection.statement != statement {
		t.Fatalf("statement = %q, want %q", connection.statement, statement)
	}
	if strings.Contains(connection.statement, "org-1") {
		t.Fatalf("statement contains a bound value: %q", connection.statement)
	}
	if got := connection.parameters["org_id"]; got != "org-1'; DROP TABLE evidence" {
		t.Fatalf("org_id binding = %q", got)
	}
}

func TestTranslateBindings_encodes_nil_for_ClickHouse_native_nullable_value(t *testing.T) {
	// Given
	bindings := []Binding{{Name: "as_of", Value: nil}}

	// When
	parameters, err := translateBindings(bindings)

	// Then
	if err != nil {
		t.Fatalf("translateBindings() error = %v", err)
	}
	if got := parameters["as_of"]; got != `\\N` {
		t.Fatalf("as_of parameter = %q, want %q", got, `\\N`)
	}
	// clickhouse-go quotes native-protocol parameter values; this transport
	// escape decodes to ClickHouse's one-backslash NULL marker on the server.
	if got := []byte(parameters["as_of"]); len(got) != 3 || got[0] != '\\' || got[1] != '\\' || got[2] != 'N' {
		t.Fatalf("as_of parameter bytes = %x, want 5c5c4e", got)
	}
}

func TestTranslateBindings_formats_DateTime64_values_in_UTC(t *testing.T) {
	// Given
	bindings := []Binding{{
		Name:  "as_of",
		Value: time.Date(2026, time.July, 10, 13, 0, 0, 123_456_789, time.FixedZone("UTC+2", 2*60*60)),
	}}

	// When
	parameters, err := translateBindings(bindings)

	// Then
	if err != nil {
		t.Fatalf("translateBindings() error = %v", err)
	}
	if got := parameters["as_of"]; got != "2026-07-10 11:00:00.123" {
		t.Fatalf("as_of parameter = %q, want DateTime64 UTC milliseconds", got)
	}
}

func TestClient_Query_closes_rows_and_propagates_close_error(t *testing.T) {
	// Given
	closeErr := errors.New("close failed")
	connection := &fakeConnection{rows: &fakeRows{closeErr: closeErr}}
	client := newClient(connection, time.Second)

	// When
	rows, err := client.Query(context.Background(), "SELECT 1", nil)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	err = rows.Close()

	// Then
	if !errors.Is(err, closeErr) {
		t.Fatalf("Close() error = %v, want %v", err, closeErr)
	}
	if !connection.rows.closed {
		t.Fatal("rows were not closed")
	}
}

func TestClient_Query_keeps_timeout_context_alive_until_rows_close(t *testing.T) {
	// Given
	connection := &fakeConnection{}
	client := newClient(connection, time.Second)

	// When
	rows, err := client.Query(context.Background(), "SELECT 1", nil)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	beforeClose := connection.context.Err()
	closeErr := rows.Close()
	afterClose := connection.context.Err()

	// Then
	if beforeClose != nil {
		t.Fatalf("query context was cancelled before row close: %v", beforeClose)
	}
	if _, ok := connection.context.Deadline(); !ok {
		t.Fatal("query context has no configured timeout")
	}
	if closeErr != nil {
		t.Fatalf("Close() error = %v", closeErr)
	}
	if !errors.Is(afterClose, context.Canceled) {
		t.Fatalf("query context after Close() = %v, want context.Canceled", afterClose)
	}
}

func TestClient_Ping_and_Close_delegate_to_connection(t *testing.T) {
	// Given
	connection := &fakeConnection{}
	client := newClient(connection)

	// When
	pingErr := client.Ping(context.Background())
	closeErr := client.Close()

	// Then
	if pingErr != nil || closeErr != nil {
		t.Fatalf("Ping() = %v, Close() = %v", pingErr, closeErr)
	}
	if !connection.pinged || !connection.closed {
		t.Fatalf("pinged=%t closed=%t", connection.pinged, connection.closed)
	}
}

func TestClient_Query_rejects_writes_and_multi_statements(t *testing.T) {
	tests := []string{
		"INSERT INTO evidence VALUES ('x')",
		"SELECT 1; SELECT 2",
		"SELECT * FROM url('https://example.invalid/evidence')",
		"SELECT 1 INTO OUTFILE '/tmp/evidence'",
	}
	for _, statement := range tests {
		t.Run(statement, func(t *testing.T) {
			// Given
			connection := &fakeConnection{}
			client := newClient(connection)

			// When
			_, err := client.Query(context.Background(), statement, nil)

			// Then
			if !errors.Is(err, ErrUnsafeStatement) {
				t.Fatalf("Query() error = %v, want ErrUnsafeStatement", err)
			}
			if connection.statement != "" {
				t.Fatalf("unsafe statement reached the connection: %q", connection.statement)
			}
		})
	}
}

func TestClient_Query_respects_cancellation_and_timeout(t *testing.T) {
	// Given
	connection := &fakeConnection{queryErr: context.Canceled}
	client := newClient(connection)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// When
	_, err := client.Query(ctx, "SELECT 1", nil)

	// Then
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Query() error = %v, want context.Canceled", err)
	}
}

func TestClient_Query_preserves_deadlines_for_execution_limits(t *testing.T) {
	// Given
	connection := &fakeConnection{}
	client := newClient(connection)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// When
	_, err := client.Query(ctx, "SELECT 1", nil)

	// Then
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if _, ok := connection.context.Deadline(); !ok {
		t.Fatal("query context has no deadline for the execution limit")
	}
}

func TestClient_Query_redacts_connection_errors(t *testing.T) {
	// Given
	const secret = "super-secret-password"
	connection := &fakeConnection{queryErr: errors.New("dial clickhouse://user:" + secret + "@host failed")}
	client := newClient(connection)

	// When
	_, err := client.Query(context.Background(), "SELECT 1", nil)

	// Then
	if err == nil {
		t.Fatal("Query() error = nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked credentials: %v", err)
	}
}

func TestNewClickHouseQueryClientWithOptions_redacts_credentials_in_errors(t *testing.T) {
	// Given
	const secret = "super-secret-password"

	// When
	_, err := NewClickHouseQueryClientWithOptions(Options{DSN: "://bad:" + secret + "@"})

	// Then
	if err == nil {
		t.Fatal("NewClickHouseQueryClientWithOptions() error = nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked credentials: %v", err)
	}
}

type fakeConnection struct {
	statement  string
	parameters map[string]string
	context    context.Context
	rows       *fakeRows
	queryErr   error
	pinged     bool
	closed     bool
}

func (c *fakeConnection) Query(ctx context.Context, statement string, parameters map[string]string) (RowScanner, error) {
	c.context = ctx
	c.statement = statement
	c.parameters = parameters
	if c.queryErr != nil {
		return nil, c.queryErr
	}
	if c.rows == nil {
		c.rows = &fakeRows{}
	}
	return c.rows, nil
}

func (c *fakeConnection) Ping(context.Context) error {
	c.pinged = true
	return nil
}

func (c *fakeConnection) Close() error {
	c.closed = true
	return nil
}

type fakeRows struct {
	closeErr error
	closed   bool
}

func (r *fakeRows) Next() bool        { return false }
func (r *fakeRows) Scan(...any) error { return nil }
func (r *fakeRows) Err() error        { return nil }
func (r *fakeRows) Close() error      { r.closed = true; return r.closeErr }
