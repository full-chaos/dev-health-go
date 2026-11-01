package authverify

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestRetireAfterFollowsTheRFC3339Grammar(t *testing.T) {
	utc := func(layoutTime string) time.Time {
		at, err := time.Parse(time.RFC3339Nano, layoutTime)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	base := utc("2026-11-02T10:30:00Z")
	leap := utc("2016-12-31T23:59:59.999999999Z")
	accept := map[string]time.Time{
		"2026-11-02T10:30:00Z":                                     base,
		"2026-11-02T10:30:00+00:00":                                base,
		"2026-11-02T10:30:00-00:00":                                base,
		"2026-11-02t10:30:00Z":                                     base,
		"2026-11-02T10:30:00z":                                     base,
		"2026-11-02t10:30:00z":                                     base,
		"2026-11-02T10:30:00.1Z":                                   base.Add(100 * time.Millisecond),
		"2026-11-02T10:30:00.123456789+00:00":                      base.Add(123456789 * time.Nanosecond),
		"2026-11-02T10:30:00.000000001-00:00":                      base.Add(time.Nanosecond),
		"2026-11-02T10:30:00.1234567899Z":                          base.Add(123456789 * time.Nanosecond),
		"2026-11-02T10:30:00.123456789012345678901234567890+00:00": base.Add(123456789 * time.Nanosecond),
		"2026-11-02T10:30:00.9999999999Z":                          base.Add(999999999 * time.Nanosecond),
		"2016-12-31T23:59:60.12345678901234Z":                      leap,
		"2016-12-31T23:59:60Z":                                     leap,
		"2016-12-31T23:59:60+00:00":                                leap,
		"2016-12-31t23:59:60z":                                     leap,
		"2016-12-31T23:59:60.5Z":                                   leap,
		"2016-12-31T23:59:60.123456789-00:00":                      leap,
	}
	for text, want := range accept {
		t.Run("accept "+text, func(t *testing.T) {
			path, _ := retireAfterDoc(t, fmt.Sprintf(`"retire_after":%q`, text))
			set, err := NewEd25519JWKSVerifier(path).KeySet()
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if got := set["old"].RetireAfter; got == nil || !got.Equal(want) || got.Location() != time.UTC {
				t.Fatalf("retire_after = %v, want %v UTC", got, want)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseJWKS(raw); !errors.Is(err, ErrInvalidJWKS) {
				t.Fatalf("HTTP path err = %v, want ErrInvalidJWKS", err)
			}
		})
	}
	refuse := []string{
		"2026-11-02T10:30:00,5Z",
		"2026-11-02T10:30:00,5+00:00",
		"2026-11-02T10:30:00,5-00:00",
		"2026-11-02T10:30:00.Z",
		"2026-11-02T10:30:00.+00:00",
		"2026-11-02T10:30:60Z",
		"2026-11-02T23:58:60Z",
		"2026-11-02T10:30:61Z",
		"2026-11-02T10:30:00+01:00",
		"2026-11-02T10:30:00-05:00",
		"2026-11-02T10:30:00+00:01",
		"2026-11-02T10:30:00",
		"2026-11-02 10:30:00Z",
		"2026-11-02T10:30Z",
		" 2026-11-02T10:30:00Z",
		"2026-11-02T10:30:00Z ",
		"2026-11-02T24:00:00Z",
		"2026-13-02T10:30:00Z",
	}
	for _, text := range refuse {
		t.Run("refuse "+text, func(t *testing.T) {
			path, _ := retireAfterDoc(t, fmt.Sprintf(`"retire_after":%q`, text))
			if _, err := NewEd25519JWKSVerifier(path).KeySet(); !errors.Is(err, ErrInvalidJWKS) {
				t.Fatalf("err = %v, want ErrInvalidJWKS", err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseJWKS(raw); !errors.Is(err, ErrInvalidJWKS) {
				t.Fatalf("HTTP path err = %v, want ErrInvalidJWKS", err)
			}
		})
	}
}
