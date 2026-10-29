package workloadtoken

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNewLogsTheExchangeSchemeAndHostOnce: one startup line names the scheme and the host the subject will be sent to;
// exchanges, refusals and outages log nothing, and no line carries a subject, a token or the subject's length.
func TestNewLogsTheExchangeSchemeAndHostOnce(t *testing.T) {
	for name, tc := range map[string]struct {
		base     string
		plain    bool
		scheme   string
		hostPort string
	}{
		"https": {"https://auth-service.test:8095", false, "https", "auth-service.test:8095"},
		"http":  {"http://auth-service:8095", true, "http", "auth-service:8095"},
	} {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			// No time attribute: the length check below must not match a time stamp.
			logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				if len(groups) == 0 && a.Key == slog.TimeKey {
					return slog.Attr{}
				}
				return a
			}}))
			calls := 0
			exchange := &fakeExchange{answer: func(n int, _ url.Values) (*http.Response, error) {
				calls++
				switch n {
				case 0:
					return issued(mintToken(0, 300), 300), nil
				case 1:
					return jsonResponse(400, `{"error":"invalid_grant"}`), nil
				default:
					return jsonResponse(503, `{"error":"temporarily_unavailable"}`), nil
				}
			}}
			path := writeSubject(t, testSubject)
			c, err := New(Config{ExchangeURL: tc.base, AllowPlainHTTP: tc.plain, Subject: KubernetesTokenFile(path),
				Transport: exchange, Logger: logger, BackoffFloor: 1, BackoffCeiling: 1})
			if err != nil {
				t.Fatal(err)
			}
			for _, audience := range []string{"a1", "a2", "a3"} {
				_, _ = c.Token(context.Background(), audience)
			}
			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			if len(lines) != 1 || calls != 3 {
				t.Fatalf("%d log lines over %d exchanges, want exactly 1 startup line:\n%s", len(lines), calls, logs.String())
			}
			if !strings.Contains(lines[0], "scheme="+tc.scheme) || !strings.Contains(lines[0], "host="+tc.hostPort) {
				t.Fatalf("the startup line lacks the scheme and host: %s", lines[0])
			}
			for _, banned := range []string{testSubject, tokenN(0), path} {
				if strings.Contains(logs.String(), banned) {
					t.Fatalf("a log line carries %q:\n%s", banned, logs.String())
				}
			}
			// The length is matched as a whole field value, never as a substring of another value.
			length := strconv.Itoa(len(testSubject))
			for _, field := range strings.Fields(lines[0]) {
				if _, value, ok := strings.Cut(field, "="); ok && strings.Trim(value, `"`) == length {
					t.Fatalf("a log field carries the subject length %s: %s", length, lines[0])
				}
			}
		})
	}
}

// TestAWritableSubjectFileIsRefused: a subject file that group or other may write is refused (someone else could swap the
// credential); read bits are allowed (a Docker or Kubernetes secret mount is 0444), and a symlink to an allowed file is followed.
func TestAWritableSubjectFileIsRefused(t *testing.T) {
	for _, tc := range []struct {
		mode os.FileMode
		ok   bool
	}{
		{0o400, true}, {0o440, true}, {0o444, true}, {0o600, true}, {0o644, true},
		{0o620, false}, {0o602, false}, {0o666, false}, {0o622, false},
	} {
		t.Run(tc.mode.String(), func(t *testing.T) {
			path := writeSubject(t, testSubject)
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			for _, via := range []string{"direct", "symlink"} {
				target := path
				if via == "symlink" {
					target = filepath.Join(t.TempDir(), "link")
					if err := os.Symlink(path, target); err != nil {
						t.Fatal(err)
					}
				}
				exchange := &fakeExchange{answer: issueEach(300)}
				c := newTestClient(t, exchange, target)
				_, err := c.Token(context.Background(), testAudience)
				if tc.ok {
					if err != nil {
						t.Fatalf("%s mode %v: %v", via, tc.mode, err)
					}
					continue
				}
				mustNoUsable(t, err, ErrSubjectUnreadable)
				if exchange.count() != 0 {
					t.Fatalf("%s mode %v: a writable subject reached the exchange", via, tc.mode)
				}
			}
		})
	}
}
