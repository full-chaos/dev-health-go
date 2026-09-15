package readers_test

import (
	"strings"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

func TestSettingsRender(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		s    readers.Settings
		want string
	}{
		{name: "zero value renders nothing", s: readers.Settings{}, want: ""},
		{
			name: "MaxExecutionTimeSeconds alone",
			s:    readers.Settings{MaxExecutionTimeSeconds: 30},
			want: "\nSETTINGS max_execution_time = 30, timeout_overflow_mode = 'throw'",
		},
		{
			name: "MaxRowsToRead alone",
			s:    readers.Settings{MaxRowsToRead: 100000},
			want: "\nSETTINGS max_rows_to_read = 100000, read_overflow_mode = 'throw'",
		},
		{
			name: "MaxMemoryUsage alone -- no overflow-mode setting exists for it",
			s:    readers.Settings{MaxMemoryUsage: 500000000},
			want: "\nSETTINGS max_memory_usage = 500000000",
		},
		{
			name: "MaxThreads alone",
			s:    readers.Settings{MaxThreads: 1},
			want: "\nSETTINGS max_threads = 1",
		},
		{
			name: "MaxResultRows alone",
			s:    readers.Settings{MaxResultRows: 201},
			want: "\nSETTINGS max_result_rows = 201, result_overflow_mode = 'throw'",
		},
		{
			name: "every field, fixed order regardless of struct literal order",
			s: readers.Settings{
				MaxResultRows:           201,
				MaxMemoryUsage:          500000000,
				MaxThreads:              1,
				MaxRowsToRead:           100000,
				MaxExecutionTimeSeconds: 30,
			},
			want: "\nSETTINGS max_execution_time = 30, timeout_overflow_mode = 'throw', " +
				"max_rows_to_read = 100000, read_overflow_mode = 'throw', " +
				"max_memory_usage = 500000000, " +
				"max_threads = 1, " +
				"max_result_rows = 201, result_overflow_mode = 'throw'",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.s.Render(); got != tt.want {
				t.Fatalf("Render() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWithSettingsIsANoopAtTheZeroValue(t *testing.T) {
	t.Parallel()
	statement := "SELECT 1"
	if got := readers.WithSettings(statement, readers.Settings{}); got != statement {
		t.Fatalf("WithSettings() = %q, want the statement unchanged: %q", got, statement)
	}
}

func TestWithSettingsAppendsTheClause(t *testing.T) {
	t.Parallel()
	statement := readers.WithSettings("SELECT 1", readers.Settings{MaxResultRows: 5})
	want := "SELECT 1\nSETTINGS max_result_rows = 5, result_overflow_mode = 'throw'"
	if statement != want {
		t.Fatalf("WithSettings() = %q, want %q", statement, want)
	}
}

func TestWithSettingsEmitsMaxThreadsOnce(t *testing.T) {
	t.Parallel()
	statement := readers.WithSettings("SELECT 1", readers.Settings{MaxThreads: 1})
	if got := strings.Count(statement, "max_threads"); got != 1 {
		t.Fatalf("max_threads occurrence count = %d, want 1 in %q", got, statement)
	}
}
