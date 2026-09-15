package readers

import "strconv"

// Settings is a per-statement ClickHouse query-complexity ceiling, rendered
// as a trailing SETTINGS clause. Every field is optional: its zero value
// leaves that particular setting out of the rendered clause entirely --
// ClickHouse's own "0 means unrestricted" reads very differently from "not
// sent" (see clickhouse.Options's own resolveCeilingUint doc comment for
// the connection-level version of the same distinction), so a reader that
// wants no per-statement ceiling at all should pass the zero value,
// Settings{}, which renders no clause and leaves the statement unchanged.
//
// Every overflow mode this type can render is 'throw', never the server's
// default 'break': a statement that would exceed one of these ceilings
// must fail with a scannable error, not silently hand back a truncated
// result that looks complete. MaxMemoryUsage carries no overflow-mode
// setting of its own -- ClickHouse has none to configure for it, a memory
// budget breach always throws.
// MaxThreads is a positive per-statement ClickHouse thread setting; it records
// the requested server setting and does not promise a particular worker count.
// Use named fields when constructing Settings so new optional fields do not
// break the construction or change the meaning of existing values.
type Settings struct {
	MaxExecutionTimeSeconds uint64
	MaxRowsToRead           uint64
	MaxMemoryUsage          uint64
	MaxThreads              uint64
	MaxResultRows           uint64
}

// Render returns the trailing " SETTINGS ..." clause for s, or "" when s is
// the zero value. Field order is fixed (MaxExecutionTimeSeconds,
// MaxRowsToRead, MaxMemoryUsage, MaxThreads, MaxResultRows) so two calls with
// the same Settings value always render byte-identical text.
func (s Settings) Render() string {
	var parts []string
	if s.MaxExecutionTimeSeconds != 0 {
		parts = append(parts,
			"max_execution_time = "+strconv.FormatUint(s.MaxExecutionTimeSeconds, 10),
			"timeout_overflow_mode = 'throw'")
	}
	if s.MaxRowsToRead != 0 {
		parts = append(parts,
			"max_rows_to_read = "+strconv.FormatUint(s.MaxRowsToRead, 10),
			"read_overflow_mode = 'throw'")
	}
	if s.MaxMemoryUsage != 0 {
		parts = append(parts,
			"max_memory_usage = "+strconv.FormatUint(s.MaxMemoryUsage, 10))
	}
	if s.MaxThreads != 0 {
		parts = append(parts,
			"max_threads = "+strconv.FormatUint(s.MaxThreads, 10))
	}
	if s.MaxResultRows != 0 {
		parts = append(parts,
			"max_result_rows = "+strconv.FormatUint(s.MaxResultRows, 10),
			"result_overflow_mode = 'throw'")
	}
	if len(parts) == 0 {
		return ""
	}
	clause := "\nSETTINGS "
	for i, part := range parts {
		if i > 0 {
			clause += ", "
		}
		clause += part
	}
	return clause
}

// WithSettings appends settings' SETTINGS clause to statement. It is a
// no-op for the zero value, mirroring WithRowLimit's own additive
// contract: a caller that never sets a ceiling gets the exact statement it
// would have gotten before this existed.
func WithSettings(statement string, settings Settings) string {
	return statement + settings.Render()
}
