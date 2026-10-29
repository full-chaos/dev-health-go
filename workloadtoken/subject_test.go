package workloadtoken

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestTheSubjectFileIsReadForEveryExchange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		data := filepath.Join(dir, "..data")
		if err := os.Mkdir(data, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(data, "token")
		if err := os.WriteFile(target, []byte(testSubject), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "token")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		exchange := &fakeExchange{answer: issueEach(300)}
		c := newTestClient(t, exchange, link)
		start := time.Now()
		if _, err := c.Token(context.Background(), testAudience); err != nil {
			t.Fatalf("through the projected-volume symlink: %v", err)
		}
		rotated := "eyJhbGciOiJSUzI1NiJ9.c3ViamVjdC12Mg.c2lnbmF0dXJl"
		if err := os.WriteFile(target, []byte(rotated), 0o600); err != nil {
			t.Fatal(err)
		}
		sleepUntil(start, 150*time.Second)
		if _, err := c.Token(context.Background(), testAudience); err != nil {
			t.Fatal(err)
		}
		if got := exchange.call(t, 0).form.Get("subject_token"); got != testSubject {
			t.Fatalf("first exchange sent %q", got)
		}
		if got := exchange.call(t, 1).form.Get("subject_token"); got != rotated {
			t.Fatalf("the exchange after the rotation sent the old subject: %q", got)
		}
	})
}

func TestAnUnreadableSubjectIsNeverSent(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"missing":   filepath.Join(dir, "absent"),
		"directory": dir,
		"empty":     writeSubject(t, ""),
		"newline":   writeSubject(t, "\n"),
		"oversize":  writeSubject(t, "eyJh."+strings.Repeat("a", maxSubjectBytes)+".c2ln"),
		"not a JWS": writeSubject(t, "svc_static_secret_value"),
		"two lines": writeSubject(t, testSubject+"\n"+testSubject),
		"spaces":    writeSubject(t, " "+testSubject),
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			exchange := &fakeExchange{answer: issueEach(300)}
			c := newTestClient(t, exchange, path)
			_, err := c.Token(context.Background(), testAudience)
			mustNoUsable(t, err, ErrSubjectUnreadable)
			if exchange.count() != 0 {
				t.Fatal("an unreadable subject reached the exchange")
			}
		})
	}
	t.Run("at the size bound", func(t *testing.T) {
		value := "eyJh." + strings.Repeat("a", maxSubjectBytes-10) + ".c2ln"
		c := newTestClient(t, &fakeExchange{answer: issueEach(300)}, writeSubject(t, value+"\n"))
		if _, err := c.Token(context.Background(), testAudience); err != nil {
			t.Fatalf("a %d-byte subject: %v", len(value), err)
		}
	})
}
