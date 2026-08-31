package clickhouse

import (
	"crypto/tls"
	"maps"
	"time"

	clickhousedriver "github.com/ClickHouse/clickhouse-go/v2"
)

const defaultMaxExecutionTime uint = 10

// DefaultMaxBytesToRead is the read-budget ceiling applied when
// Options.MaxBytesToRead is nil (unset). CHAOS-3848: the previous 16 MiB
// default was sized before CHAOS-3833's enriched projection rows (PR body
// heads, labels, descriptions) pushed a routine 200-row pull_requests batch
// to ~17 MiB of granule reads, which ClickHouse rejects with Code 307
// (TOO_MANY_BYTES) on every retry -- a permanent wedge, not a transient one.
// 64 MiB is four times the old ceiling: real headroom for v2 row widths
// while remaining a genuine guard against an unbounded scan. A caller that
// wants no ceiling at all must say so explicitly (Options.MaxBytesToRead =
// new(uint64)); this default only fills in for a caller who set nothing.
const DefaultMaxBytesToRead uint64 = 64 << 20

func applyOptions(configured *clickhousedriver.Options, options Options) {
	configured.TLS = mergeTLS(options.TLS, configured.TLS)
	configured.DialTimeout = optionDuration(options.DialTimeout, configured.DialTimeout, 5*time.Second)
	configured.ReadTimeout = optionDuration(options.ReadTimeout, configured.ReadTimeout, 15*time.Second)
	configured.MaxOpenConns = optionPositive(options.MaxOpenConns, configured.MaxOpenConns, 8)
	configured.MaxIdleConns = optionPositive(options.MaxIdleConns, configured.MaxIdleConns, 4)
	configured.ConnMaxLifetime = optionDuration(options.ConnMaxLifetime, configured.ConnMaxLifetime, 30*time.Minute)
	if configured.Compression == nil {
		configured.Compression = &clickhousedriver.Compression{Method: clickhousedriver.CompressionZSTD}
	}
	configured.Settings = cloneSettings(configured.Settings)
	configured.Settings["max_execution_time"] = maxExecutionTime(options)
	configured.Settings["max_result_rows"] = resolveCeilingUint(options.MaxResultRows, 1_000)
	configured.Settings["max_bytes_to_read"] = resolveCeilingUint64(options.MaxBytesToRead, DefaultMaxBytesToRead)
}

func queryTimeoutForOptions(options Options) time.Duration {
	limit := executionDuration(maxExecutionTime(options))
	if options.QueryTimeout > 0 && options.QueryTimeout < limit {
		return options.QueryTimeout
	}
	return limit
}

func maxExecutionTime(options Options) uint {
	return defaultPositiveUint(options.MaxExecutionTime, defaultMaxExecutionTime)
}

func executionDuration(seconds uint) time.Duration {
	const maxDuration = time.Duration(1<<63 - 1)
	value := time.Duration(seconds)
	if value <= 0 || value > maxDuration/time.Second {
		return maxDuration
	}
	return value * time.Second
}

func cloneSettings(settings clickhousedriver.Settings) clickhousedriver.Settings {
	cloned := make(clickhousedriver.Settings, len(settings)+4)
	maps.Copy(cloned, settings)
	return cloned
}

func mergeTLS(preferred, fallback *tls.Config) *tls.Config {
	if preferred != nil {
		merged := preferred.Clone()
		if fallback != nil && merged.ServerName == "" {
			merged.ServerName = fallback.ServerName
		}
		return merged
	}
	if fallback != nil {
		return fallback.Clone()
	}
	return nil
}

func defaultDuration(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func defaultPositive(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

func optionDuration(value, configured, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return defaultDuration(configured, fallback)
}

func optionPositive(value, configured, fallback int) int {
	if value > 0 {
		return value
	}
	return defaultPositive(configured, fallback)
}

func defaultPositiveUint(value, fallback uint) uint {
	if value > 0 {
		return value
	}
	return fallback
}

// resolveCeilingUint resolves a ClickHouse "maximum X" query-complexity
// setting where 0 is a meaningful, distinct value: ClickHouse defines 0 as
// "unrestricted" for these settings (query-complexity docs: "Restrictions on
// the 'maximum amount of something' can take a value of 0, which means that
// it is 'unrestricted'"), and this package forwards whatever is configured
// verbatim (see conn.go in clickhouse-go/v2, which sends every key in
// Options.Settings unconditionally -- no special-casing of 0).
//
// nil means the caller did not set the option: use fallback. A non-nil
// pointer -- INCLUDING one pointing at 0 -- is the caller's explicit choice
// and must reach the driver unchanged. Do not collapse this back to "any
// value <=0 substitutes the fallback" (that was CHAOS-4651: it made
// "unlimited" inexpressible, and a caller deleting the field to remove a
// ceiling silently got the default restored instead).
func resolveCeilingUint(value *uint, fallback uint) uint {
	if value == nil {
		return fallback
	}
	return *value
}

// resolveCeilingUint64 is resolveCeilingUint's uint64 counterpart, for
// Options.MaxBytesToRead. See resolveCeilingUint for the full rationale.
func resolveCeilingUint64(value *uint64, fallback uint64) uint64 {
	if value == nil {
		return fallback
	}
	return *value
}
