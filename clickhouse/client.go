package clickhouse

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	clickhousedriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

var (
	ErrInvalidConfiguration = errors.New("clickhouse runtime: invalid configuration")
	ErrUnsafeStatement      = errors.New("clickhouse runtime: unsafe statement")
	ErrInvalidBinding       = errors.New("clickhouse runtime: invalid binding")
)

var (
	externalTableFunction = regexp.MustCompile(`(?i)\b(url|s3|remote|file|hdfs|mysql|postgresql|sqlite)\s*\(`)
	outputClause          = regexp.MustCompile(`(?i)\binto\s+(outfile|dumpfile)\b`)
)

// Binding is a single named parameter bound into a read-only ClickHouse
// statement. It is a generic SQL-row-scanning primitive: it carries no
// caller-specific shape and can be constructed by any consumer of this
// package.
type Binding struct {
	Name  string
	Value any
}

// RowScanner is the narrow query boundary needed by callers of Client.Query:
// it supports both real driver implementations and lightweight fakes without
// exposing a driver.
type RowScanner interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close() error
}

type Options struct {
	DSN              string
	TLS              *tls.Config
	DialTimeout      time.Duration
	ReadTimeout      time.Duration
	QueryTimeout     time.Duration
	MaxOpenConns     int
	MaxIdleConns     int
	ConnMaxLifetime  time.Duration
	MaxExecutionTime uint
	MaxResultRows    uint
	MaxBytesToRead   uint64
}

type Client struct {
	connection   queryConnection
	queryTimeout time.Duration
}

type queryConnection interface {
	Query(context.Context, string, map[string]string) (RowScanner, error)
	Ping(context.Context) error
	Close() error
}

type nativeConnection struct{ connection driver.Conn }

func NewClickHouseQueryClientWithOptions(options Options) (*Client, error) {
	if strings.TrimSpace(options.DSN) == "" {
		return nil, ErrInvalidConfiguration
	}
	connectionURL, err := url.Parse(options.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse ClickHouse connection: %w", ErrInvalidConfiguration)
	}
	configured, err := clickhousedriver.ParseDSN(options.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse ClickHouse connection: %w", ErrInvalidConfiguration)
	}
	if _, configuredByDSN := configured.Settings["readonly"]; configuredByDSN {
		return nil, fmt.Errorf("validate ClickHouse settings: %w", ErrInvalidConfiguration)
	}
	applyOptions(configured, options)
	if strings.EqualFold(connectionURL.Scheme, "http") && configured.TLS != nil {
		return nil, fmt.Errorf("validate ClickHouse TLS: %w", ErrInvalidConfiguration)
	}
	if configured.TLS != nil && configured.TLS.InsecureSkipVerify {
		return nil, fmt.Errorf("validate ClickHouse TLS: %w", ErrInvalidConfiguration)
	}
	connection, err := clickhousedriver.Open(configured)
	if err != nil {
		return nil, fmt.Errorf("open ClickHouse connection: %w", ErrInvalidConfiguration)
	}
	return newClient(nativeConnection{connection: connection}, queryTimeoutForOptions(options)), nil
}

func newClient(connection queryConnection, queryTimeout ...time.Duration) *Client {
	var timeout time.Duration
	if len(queryTimeout) > 0 {
		timeout = queryTimeout[0]
	}
	return &Client{connection: connection, queryTimeout: timeout}
}

func (c *Client) Query(ctx context.Context, statement string, bindings []Binding) (RowScanner, error) {
	if err := validateReadOnlyStatement(statement); err != nil {
		return nil, err
	}
	parameters, err := translateBindings(bindings)
	if err != nil {
		return nil, err
	}
	if c.queryTimeout > 0 {
		ctx, cancel := context.WithTimeout(ctx, c.queryTimeout)
		rows, err := c.connection.Query(ctx, statement, parameters)
		if err != nil {
			cancel()
			return nil, &operationError{operation: "query", cause: err}
		}
		return &managedRows{rows: rows, cancel: cancel}, nil
	}
	rows, err := c.connection.Query(ctx, statement, parameters)
	if err != nil {
		return nil, &operationError{operation: "query", cause: err}
	}
	return rows, nil
}

func (c *Client) Ping(ctx context.Context) error {
	if err := c.connection.Ping(ctx); err != nil {
		return &operationError{operation: "ping", cause: err}
	}
	return nil
}

func (c *Client) Close() error {
	if err := c.connection.Close(); err != nil {
		return &operationError{operation: "close", cause: err}
	}
	return nil
}

func (c nativeConnection) Query(ctx context.Context, statement string, parameters map[string]string) (RowScanner, error) {
	return c.connection.Query(clickhousedriver.Context(ctx, clickhousedriver.WithParameters(parameters)), statement)
}

func (c nativeConnection) Ping(ctx context.Context) error { return c.connection.Ping(ctx) }
func (c nativeConnection) Close() error                   { return c.connection.Close() }

type managedRows struct {
	rows   RowScanner
	cancel context.CancelFunc
}

func (r *managedRows) Next() bool {
	next := r.rows.Next()
	if !next {
		r.cancel()
	}
	return next
}

func (r *managedRows) Scan(dest ...any) error { return r.rows.Scan(dest...) }

func (r *managedRows) Err() error {
	if err := r.rows.Err(); err != nil {
		return &operationError{operation: "row iteration", cause: err}
	}
	return nil
}

func (r *managedRows) Close() error {
	defer r.cancel()
	if err := r.rows.Close(); err != nil {
		return &operationError{operation: "row close", cause: err}
	}
	return nil
}

func validateReadOnlyStatement(statement string) error {
	if strings.Contains(statement, ";") {
		return ErrUnsafeStatement
	}
	if !strings.EqualFold(firstToken(statement), "SELECT") {
		return ErrUnsafeStatement
	}
	if externalTableFunction.MatchString(statement) || outputClause.MatchString(statement) {
		return ErrUnsafeStatement
	}
	return nil
}

func firstToken(statement string) string {
	fields := strings.Fields(statement)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

type operationError struct {
	operation string
	cause     error
}

func (e *operationError) Error() string { return "ClickHouse " + e.operation + " failed" }
func (e *operationError) Unwrap() error { return e.cause }
