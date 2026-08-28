package clickhouse

import (
	"context"
	"testing"
	"time"
)

func TestClient_Query_caps_request_deadline_at_max_execution_time(t *testing.T) {
	// Given
	connection := &fakeConnection{}
	client := newClient(connection, queryTimeoutForOptions(Options{MaxExecutionTime: 2, QueryTimeout: 15 * time.Second}))
	requestContext, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	requestDeadline, _ := requestContext.Deadline()

	// When
	rows, err := client.Query(requestContext, "SELECT 1", nil)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	defer rows.Close()
	deadline, ok := connection.context.Deadline()

	// Then
	if !ok {
		t.Fatal("query context has no deadline")
	}
	if deadline.After(requestDeadline.Add(-10 * time.Second)) {
		t.Fatalf("query deadline = %v, want no later than configured 2s execution limit", deadline)
	}
}

func TestQueryTimeoutForOptions_preserves_shorter_client_timeout(t *testing.T) {
	// Given
	options := Options{MaxExecutionTime: 10, QueryTimeout: time.Second}

	// When
	timeout := queryTimeoutForOptions(options)

	// Then
	if timeout != time.Second {
		t.Fatalf("query timeout = %v, want shorter client timeout", timeout)
	}
}
