//go:build unix

package workloadtoken

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestAFIFOSubjectPathNeverBlocks: a subject path that is a FIFO (no writer) is refused at once, for both subjects; the
// open does not wait for a writer. The 30 s wait is a guard for the failure path only (a passing run returns at once).
func TestAFIFOSubjectPathNeverBlocks(t *testing.T) {
	for _, kind := range []string{"kubernetes", "bootstrap"} {
		t.Run(kind, func(t *testing.T) {
			fifo := filepath.Join(t.TempDir(), "subject")
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Skipf("mkfifo: %v", err)
			}
			// If the open blocks, a writer releases it at the end so the test binary does not hang.
			t.Cleanup(func() {
				if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					_ = w.Close()
				}
			})
			exchange := &fakeExchange{answer: issueEach(300)}
			var c *Client
			if kind == "kubernetes" {
				c = newTestClient(t, exchange, fifo)
			} else {
				c = newBootstrapClient(t, exchange, fifo)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := c.Token(ctx, testAudience)
				done <- err
			}()
			select {
			case err := <-done:
				mustNoUsable(t, err, ErrSubjectUnreadable)
			case <-time.After(30 * time.Second):
				t.Fatal("Token is blocked opening a FIFO subject path")
			}
			if exchange.count() != 0 {
				t.Fatal("a FIFO subject reached the exchange")
			}
		})
	}
}

// TestAFIFOHoldingAValidSubjectIsRefused: a FIFO whose writer already put a valid subject in it is still not a regular
// file and is refused (a stream is not a secret file).
func TestAFIFOHoldingAValidSubjectIsRefused(t *testing.T) {
	for _, kind := range []string{"kubernetes", "bootstrap"} {
		t.Run(kind, func(t *testing.T) {
			fifo := filepath.Join(t.TempDir(), "subject")
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Skipf("mkfifo: %v", err)
			}
			holder, err := os.OpenFile(fifo, os.O_RDWR, 0)
			if err != nil {
				t.Skipf("open the FIFO read-write: %v", err)
			}
			defer holder.Close()
			value := testSubject
			if kind == "bootstrap" {
				value = testBootstrap
			}
			if _, err := holder.WriteString(value); err != nil {
				t.Fatal(err)
			}
			exchange := &fakeExchange{answer: issueEach(300)}
			var c *Client
			if kind == "kubernetes" {
				c = newTestClient(t, exchange, fifo)
			} else {
				c = newBootstrapClient(t, exchange, fifo)
			}
			done := make(chan error, 1)
			go func() {
				_, err := c.Token(context.Background(), testAudience)
				done <- err
			}()
			// The 30 s wait is a guard for the failure path only: a reader of the stream would wait for more data.
			select {
			case err := <-done:
				mustNoUsable(t, err, ErrSubjectUnreadable)
			case <-time.After(30 * time.Second):
				t.Fatal("Token is reading the FIFO stream instead of refusing it")
			}
			if exchange.count() != 0 {
				t.Fatal("a value from a FIFO reached the exchange")
			}
		})
	}
}
