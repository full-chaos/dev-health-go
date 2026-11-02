package pep

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"time"
)

// maxDepth bounds JSON nesting. A decision document nests four deep.
const maxDepth = 16

var errStrict = errors.New("pep: not a strict JSON document")

// rejectDuplicates refuses a document that repeats an object member at any
// depth, holds data after its value, or nests past maxDepth. A JSON object
// that repeats a member decodes to the last one, so {"allowed":false,
// "allowed":true} reads as an allow: only the bytes show the verdict was
// ambiguous. Member names compare after unescaping, so "allowed" repeats
// "allowed".
func rejectDuplicates(body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := walk(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errStrict
	}
	return nil
}

func walk(dec *json.Decoder, depth int) error {
	if depth > maxDepth {
		return errStrict
	}
	tok, err := dec.Token()
	if err != nil {
		return errStrict
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '{':
		seen := map[string]struct{}{}
		for dec.More() {
			kt, err := dec.Token()
			key, isString := kt.(string)
			if err != nil || !isString {
				return errStrict
			}
			if _, dup := seen[key]; dup {
				return errStrict
			}
			seen[key] = struct{}{}
			if err := walk(dec, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := walk(dec, depth+1); err != nil {
				return err
			}
		}
	}
	if _, err := dec.Token(); err != nil {
		return errStrict
	}
	return nil
}

var integerToken = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// parseRevision reads a non-negative integer token. A decimal or exponent form
// (42.0, 4.2e1) is refused: it is a schema-valid integer, but a float reader
// has already rounded it, and a rounded revision silently breaks the cache key
// it exists to be. The token is read as text and never touches float64.
func parseRevision(raw []byte) (uint64, error) {
	if !integerToken.Match(raw) {
		return 0, errStrict
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, errStrict
	}
	return uint64(n), nil
}

// The expires_at pattern of principal.v1: RFC 3339, every component in its
// range, at most six fractional digits (Go holds nanoseconds, other clients
// microseconds, so a longer fraction would mean two instants).
var expiresAtPattern = regexp.MustCompile(`^([0-9]{3}[1-9]|[0-9]{2}[1-9][0-9]|[0-9][1-9][0-9]{2}|[1-9][0-9]{3})-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]{1,6})?(Z|[+-](0[0-9]|1[0-9]|2[0-3]):[0-5][0-9])$`)

var (
	minInstant = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	maxInstant = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
)

func parseExpiry(s string) (time.Time, error) {
	if !expiresAtPattern.MatchString(s) {
		return time.Time{}, errStrict
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, errStrict
	}
	if u := t.UTC(); u.Before(minInstant) || u.After(maxInstant) {
		return time.Time{}, errStrict
	}
	return t.UTC(), nil
}
