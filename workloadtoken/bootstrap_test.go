package workloadtoken

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// Canonical bootstrap values: the prefix and 32 bytes as unpadded base64url, as the create verb mints them.
var (
	testBootstrap        = BootstrapPrefix + base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	testBootstrapRotated = BootstrapPrefix + base64.RawURLEncoding.EncodeToString([]byte("fedcba9876543210fedcba9876543210"))
)

func writeBootstrap(t *testing.T, value string, mode os.FileMode) string {
	t.Helper()
	path := writeSubject(t, value)
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func newBootstrapClient(t *testing.T, exchange *fakeExchange, path string) *Client {
	t.Helper()
	c, err := New(Config{ExchangeURL: testBase, Issuer: testIssuerURL, Subject: BootstrapCredentialFile(path), Transport: exchange})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestABootstrapCredentialIsPresentedWithItsOwnTokenType(t *testing.T) {
	exchange := &fakeExchange{answer: issueEach(300)}
	c := newBootstrapClient(t, exchange, writeBootstrap(t, testBootstrap+"\n", 0o444))
	if token, err := c.Token(context.Background(), testAudience); err != nil || token != tokenN(0) {
		t.Fatalf("token %q err %v", token, err)
	}
	form := exchange.call(t, 0).form
	if form.Get("subject_token_type") != TokenTypeWorkloadBootstrap || form.Get("subject_token") != testBootstrap {
		t.Fatalf("sent %q / %q", form.Get("subject_token_type"), form.Get("subject_token"))
	}
	if form.Get("grant_type") != GrantTypeTokenExchange || form.Get("audience") != testAudience {
		t.Fatalf("form %v", form)
	}
}

func TestTheBootstrapFileIsReadForEveryExchangeThroughASymlink(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		target := writeBootstrap(t, testBootstrap, 0o444)
		link := filepath.Join(t.TempDir(), "workload-bootstrap")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		exchange := &fakeExchange{answer: issueEach(300)}
		c := newBootstrapClient(t, exchange, link)
		start := time.Now()
		if _, err := c.Token(context.Background(), testAudience); err != nil {
			t.Fatalf("through the symlink: %v", err)
		}
		if err := os.Chmod(target, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(testBootstrapRotated), 0o600); err != nil {
			t.Fatal(err)
		}
		sleepUntil(start, 150*time.Second)
		if _, err := c.Token(context.Background(), testAudience); err != nil {
			t.Fatal(err)
		}
		if got := exchange.call(t, 1).form.Get("subject_token"); got != testBootstrapRotated {
			t.Fatal("the exchange after the rotation sent the old bootstrap value")
		}
	})
}

func TestAnUnusableBootstrapFileIsNeverSent(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"missing":                      filepath.Join(dir, "absent"),
		"directory":                    dir,
		"empty":                        writeBootstrap(t, "", 0o444),
		"group writable":               writeBootstrap(t, testBootstrap, 0o664),
		"other writable":               writeBootstrap(t, testBootstrap, 0o646),
		"no prefix":                    writeBootstrap(t, strings.TrimPrefix(testBootstrap, BootstrapPrefix), 0o444),
		"other prefix":                 writeBootstrap(t, "svc_"+strings.TrimPrefix(testBootstrap, BootstrapPrefix), 0o444),
		"one char short":               writeBootstrap(t, testBootstrap[:len(testBootstrap)-1], 0o444),
		"one char long":                writeBootstrap(t, testBootstrap+"a", 0o444),
		"padding":                      writeBootstrap(t, testBootstrap[:len(testBootstrap)-1]+"=", 0o444),
		"standard base64":              writeBootstrap(t, testBootstrap[:len(testBootstrap)-1]+"+", 0o444),
		"two lines":                    writeBootstrap(t, testBootstrap+"\n"+testBootstrap, 0o444),
		"two newlines":                 writeBootstrap(t, testBootstrap+"\n\n", 0o444),
		"carriage return":              writeBootstrap(t, testBootstrap+"\r\n", 0o444),
		"leading space":                writeBootstrap(t, " "+testBootstrap, 0o444),
		"non-canonical last character": writeBootstrap(t, BootstrapPrefix+strings.Repeat("A", 42)+"B", 0o444),
		"non-canonical trailing bits":  writeBootstrap(t, testBootstrap[:len(testBootstrap)-1]+"_", 0o444),
		"a JWT":                        writeBootstrap(t, testSubject, 0o444),
		"oversize garbage":             writeBootstrap(t, strings.Repeat("a", 1<<20), 0o444),
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			exchange := &fakeExchange{answer: issueEach(300)}
			c := newBootstrapClient(t, exchange, path)
			_, err := c.Token(context.Background(), testAudience)
			mustNoUsable(t, err, ErrSubjectUnreadable)
			if exchange.count() != 0 {
				t.Fatal("an unusable bootstrap value reached the exchange")
			}
			if strings.Contains(err.Error(), "AbCdEf") {
				t.Fatalf("the error carries the value: %v", err)
			}
		})
	}
}

func TestAKubernetesDeploymentNeverSendsABootstrapValue(t *testing.T) {
	exchange := &fakeExchange{answer: issueEach(300)}
	c := newTestClient(t, exchange, writeBootstrap(t, testBootstrap, 0o444))
	_, err := c.Token(context.Background(), testAudience)
	mustNoUsable(t, err, ErrSubjectUnreadable)
	if exchange.count() != 0 {
		t.Fatal("a bootstrap value was sent as a Kubernetes token")
	}
}

// The bootstrap constants follow the ops credential class row; testdata holds that row verbatim, extracted with
//
//	git show <ops sha>:contracts/auth/v1/credential-classes.json | python3 -c 'import json,sys; d=json.load(sys.stdin); \
//	  print(json.dumps([r for r in d["classes"] if r["class_id"]=="workload_bootstrap_credential"][0], indent=2, sort_keys=True))'
//
// at ops main ae7c981b55b08eb64670b6413f62622ff68e3fc0 (it holds the full-chaos/dev-health-ops #5215 merge 46f0604c; the row is
// byte-identical to the one taken at the PR head 0f2c923d). A change of the row is a change of this pin: update the file,
// its sha256 below and the constants together.
const opsRowSHA256 = "4de1cb69a956990ad9d8fc929c90e61bcbb7dd6e4fdc519dc5babf511b513ee1"

func TestTheBootstrapConstantsMatchTheOpsCredentialClassRow(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "ops-credential-class-workload_bootstrap_credential.json"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != opsRowSHA256 {
		t.Fatalf("the ops row file changed (sha256 %s): update the pin and the constants together", got)
	}
	var row struct {
		ClassID string `json:"class_id"`
		Markers []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"markers"`
		Transport struct {
			HeaderOrCookie string `json:"header_or_cookie"`
			Shape          string `json:"shape"`
		} `json:"transport"`
	}
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	if row.ClassID != "workload_bootstrap_credential" {
		t.Fatalf("class %q", row.ClassID)
	}
	var prefixes []string
	for _, m := range row.Markers {
		if m.Kind == "credential_prefix" {
			prefixes = append(prefixes, m.Name)
		}
	}
	if len(prefixes) != 1 || prefixes[0] != BootstrapPrefix {
		t.Fatalf("the row's credential_prefix markers %v, the client's prefix %q", prefixes, BootstrapPrefix)
	}
	if !strings.Contains(row.Transport.HeaderOrCookie, "form field subject_token") ||
		!strings.Contains(row.Transport.HeaderOrCookie, "subject_token_type "+TokenTypeWorkloadBootstrap+")") {
		t.Fatalf("the row's transport %q does not name the client's subject_token_type %q", row.Transport.HeaderOrCookie, TokenTypeWorkloadBootstrap)
	}
	shape := regexp.MustCompile(`^` + regexp.QuoteMeta(BootstrapPrefix) + `<32-byte urlsafe base64 secret> \((\d+) characters\)$`).FindStringSubmatch(row.Transport.Shape)
	if shape == nil || shape[1] != "43" || bootstrapValueBytes != len(BootstrapPrefix)+43 {
		t.Fatalf("the row's shape %q against the client's %d-byte value", row.Transport.Shape, bootstrapValueBytes)
	}
}

// TestTheTestBootstrapValuesAreCanonical: the fixtures are what the create verb mints (32 bytes, canonical encoding).
func TestTheTestBootstrapValuesAreCanonical(t *testing.T) {
	for _, value := range []string{testBootstrap, testBootstrapRotated} {
		raw, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(value, BootstrapPrefix))
		if err != nil || len(raw) != 32 || BootstrapPrefix+base64.RawURLEncoding.EncodeToString(raw) != value {
			t.Fatalf("%q is not a canonical 32-byte value", value)
		}
	}
}

// TestASecretFileAnotherUserCanChangeIsRefused: a subject file (either kind) owned by a user other than this process or
// root is refused, unless it is on a read-only mount (a Compose or Docker secret is a read-only bind mount that keeps
// the host file's owner; a Kubernetes projected volume is read-only and root-owned): nobody can change it through that
// mount. A file whose owner cannot be read is refused unless it is on a read-only mount.
func TestASecretFileAnotherUserCanChangeIsRefused(t *testing.T) {
	euid := os.Geteuid()
	for _, kind := range []string{"kubernetes", "bootstrap"} {
		for name, tc := range map[string]struct {
			owner    int
			known    bool
			readOnly bool
			ok       bool
		}{
			"owned by this process":          {euid, true, false, true},
			"owned by root":                  {0, true, false, true},
			"another owner, writable mount":  {euid + 4242, true, false, false},
			"another owner, read-only mount": {euid + 4242, true, true, true},
			"owner unknown, writable mount":  {0, false, false, false},
			"owner unknown, read-only mount": {0, false, true, true},
		} {
			t.Run(kind+" "+name, func(t *testing.T) {
				prevOwner, prevRO := ownerOf, onReadOnlyMount
				ownerOf = func(os.FileInfo) (uint32, bool) { return uint32(tc.owner), tc.known }
				onReadOnlyMount = func(*os.File) bool { return tc.readOnly }
				t.Cleanup(func() { ownerOf, onReadOnlyMount = prevOwner, prevRO })
				exchange := &fakeExchange{answer: issueEach(300)}
				var c *Client
				if kind == "kubernetes" {
					c = newTestClient(t, exchange, writeSubject(t, testSubject))
				} else {
					c = newBootstrapClient(t, exchange, writeBootstrap(t, testBootstrap, 0o444))
				}
				_, err := c.Token(context.Background(), testAudience)
				if tc.ok {
					if err != nil {
						t.Fatalf("refused: %v", err)
					}
					return
				}
				mustNoUsable(t, err, ErrSubjectUnreadable)
				if exchange.count() != 0 {
					t.Fatal("a file another user can change reached the exchange")
				}
			})
		}
	}
}

// TestTheRealOwnerAndMountAreRead: on this host the test's own temp file is owned by this process and its temp
// directory is not a read-only mount; the real seams read both.
func TestTheRealOwnerAndMountAreRead(t *testing.T) {
	path := writeSubject(t, testSubject)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if uid, ok := fileOwnerUID(info); !ok || int(uid) != os.Geteuid() {
		t.Fatalf("owner %d (known %v), want this process %d", uid, ok, os.Geteuid())
	}
	if readOnlyFile(file) {
		t.Fatal("the temp directory reads as a read-only mount")
	}
}
