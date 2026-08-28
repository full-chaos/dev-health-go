package readers_test

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/full-chaos/dev-health-go/readers"
)

// fakeTable is one canned response a fakeClient.Query call can match
// against, ported from acr's internal/contextfabric/devhealthfacts
// helpers_test.go's fakeTable/fakeClient/fakeScanner pattern (CHAOS-4377):
// that package's version is unexported inside a _test.go file, so it can't
// be imported, and this package needs the identical small fake for the
// same reason devhealthfacts's own tests do -- asserting a statement's
// text/table match and scanning canned column values without a real
// ClickHouse connection.
type fakeTable struct {
	match string
	rows  [][]any
	err   error
}

type capturedQuery struct {
	statement string
	bindings  []readers.Binding
}

type fakeClient struct {
	tables  []fakeTable
	queries []capturedQuery
}

func (c *fakeClient) Query(_ context.Context, statement string, bindings []readers.Binding) (readers.RowScanner, error) {
	c.queries = append(c.queries, capturedQuery{statement: statement, bindings: bindings})
	for _, table := range c.tables {
		if strings.Contains(statement, table.match) {
			if table.err != nil {
				return nil, table.err
			}
			return &fakeScanner{rows: table.rows}, nil
		}
	}
	return &fakeScanner{}, nil
}

func (c *fakeClient) idsBinding() []string {
	if len(c.queries) == 0 {
		return nil
	}
	last := c.queries[len(c.queries)-1]
	for _, binding := range last.bindings {
		if binding.Name == "ids" {
			if ids, ok := binding.Value.([]string); ok {
				return ids
			}
		}
	}
	return nil
}

func (c *fakeClient) orgIDBinding() string {
	if len(c.queries) == 0 {
		return ""
	}
	last := c.queries[len(c.queries)-1]
	for _, binding := range last.bindings {
		if binding.Name == "org_id" {
			if orgID, ok := binding.Value.(string); ok {
				return orgID
			}
		}
	}
	return ""
}

type fakeScanner struct {
	rows [][]any
	row  int
}

func (s *fakeScanner) Next() bool { return s.row < len(s.rows) }

func (s *fakeScanner) Scan(dest ...any) error {
	row := s.rows[s.row]
	for index, target := range dest {
		switch value := target.(type) {
		case *string:
			*value = row[index].(string)
		case *int64:
			*value = row[index].(int64)
		case *uint32:
			*value = row[index].(uint32)
		case *uint64:
			*value = row[index].(uint64)
		case *uint8:
			*value = row[index].(uint8)
		case *time.Time:
			*value = row[index].(time.Time)
		case *float64:
			*value = row[index].(float64)
		default:
			return errors.New("readers_test: unsupported scan destination")
		}
	}
	s.row++
	return nil
}

func (s *fakeScanner) Err() error   { return nil }
func (s *fakeScanner) Close() error { return nil }
