package workloadtoken

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNewRefusesAnImplicitOrProxiedTransport(t *testing.T) {
	proxyURL, _ := url.Parse("http://proxy.test:3128")
	refused := map[string]http.RoundTripper{
		"nil":                   nil,
		"DefaultTransport":      http.DefaultTransport,
		"environment proxy":     &http.Transport{Proxy: http.ProxyFromEnvironment},
		"fixed proxy":           &http.Transport{Proxy: http.ProxyURL(proxyURL)},
		"DefaultTransport copy": http.DefaultTransport.(*http.Transport).Clone(),
	}
	path := writeSubject(t, testSubject)
	for name, rt := range refused {
		t.Run(name, func(t *testing.T) {
			if _, err := New(Config{ExchangeURL: testBase, Issuer: testIssuerURL, Subject: KubernetesTokenFile(path), Transport: rt}); err == nil {
				t.Fatal("New accepted the transport")
			}
			c := newTestClient(t, &fakeExchange{answer: issueEach(300)}, path)
			if _, err := c.Transport(rt, testTarget, testAudience); err == nil {
				t.Fatal("RoundTripper accepted the transport")
			}
		})
	}
	for name, rt := range map[string]http.RoundTripper{"no proxy": &http.Transport{}, "custom": &fakeExchange{}} {
		t.Run("accept "+name, func(t *testing.T) {
			if _, err := New(Config{ExchangeURL: testBase, Issuer: testIssuerURL, Subject: KubernetesTokenFile(path), Transport: rt}); err != nil {
				t.Fatalf("New: %v", err)
			}
		})
	}
}

func TestNewRefusesABadConfig(t *testing.T) {
	path := writeSubject(t, testSubject)
	base := func() Config {
		return Config{ExchangeURL: testBase, Issuer: testIssuerURL, Subject: KubernetesTokenFile(path), Transport: &fakeExchange{}}
	}
	refused := map[string]func(*Config){
		"no subject":          func(c *Config) { c.Subject = Subject{} },
		"no issuer":           func(c *Config) { c.Issuer = "" },
		"issuer http":         func(c *Config) { c.Issuer = "http://auth-service.test" },
		"issuer with query":   func(c *Config) { c.Issuer = testIssuerURL + "?x" },
		"empty subject path":  func(c *Config) { c.Subject = KubernetesTokenFile("") },
		"no URL":              func(c *Config) { c.ExchangeURL = "" },
		"relative URL":        func(c *Config) { c.ExchangeURL = "auth-service:8095" },
		"http without opt-in": func(c *Config) { c.ExchangeURL = "http://auth-service:8095" },
		"ftp":                 func(c *Config) { c.ExchangeURL = "ftp://auth-service" },
		"user info":           func(c *Config) { c.ExchangeURL = "https://u:p@auth-service" },
		"query":               func(c *Config) { c.ExchangeURL = "https://auth-service?x=1" },
		"bare query":          func(c *Config) { c.ExchangeURL = "https://auth-service?" },
		"fragment":            func(c *Config) { c.ExchangeURL = "https://auth-service#x" },
		"path":                func(c *Config) { c.ExchangeURL = "https://auth-service/internal" },
		"timeout too short":   func(c *Config) { c.Timeout = time.Millisecond },
		"timeout too long":    func(c *Config) { c.Timeout = MaxTimeout + time.Second },
		"fraction 1":          func(c *Config) { c.RefreshFraction = 1 },
		"fraction negative":   func(c *Config) { c.RefreshFraction = -0.1 },
		"fraction above 1":    func(c *Config) { c.RefreshFraction = 1.5 },
		"margin negative":     func(c *Config) { c.SafetyMargin = -time.Second },
		"margin too large":    func(c *Config) { c.SafetyMargin = MaxSafetyMargin + time.Second },
		"backoff inverted":    func(c *Config) { c.BackoffFloor, c.BackoffCeiling = 10*time.Second, time.Second },
		"backoff too long":    func(c *Config) { c.BackoffCeiling = time.Hour },
	}
	for name, mutate := range refused {
		t.Run(name, func(t *testing.T) {
			cfg := base()
			mutate(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatal("New accepted the config")
			}
		})
	}
	for name, tc := range map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"https":            {func(*Config) {}, testBase + ExchangePath},
		"https with slash": {func(c *Config) { c.ExchangeURL = testBase + "/" }, testBase + ExchangePath},
		"http opted in":    {func(c *Config) { c.ExchangeURL, c.AllowPlainHTTP = "http://auth-service:8095", true }, "http://auth-service:8095" + ExchangePath},
	} {
		t.Run("accept "+name, func(t *testing.T) {
			cfg := base()
			tc.mutate(&cfg)
			c, err := New(cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if c.endpoint != tc.want {
				t.Fatalf("endpoint %q, want %q", c.endpoint, tc.want)
			}
		})
	}
}

func TestTheAudienceAndScopeAreBounded(t *testing.T) {
	c := newTestClient(t, &fakeExchange{answer: issueEach(300)}, writeSubject(t, testSubject))
	for name, call := range map[string]func() error{
		"empty audience":      func() error { _, err := c.Token(t.Context(), ""); return err },
		"long audience":       func() error { _, err := c.Token(t.Context(), strings.Repeat("a", 65)); return err },
		"audience space":      func() error { _, err := c.Token(t.Context(), "a b"); return err },
		"empty action":        func() error { _, err := c.Token(t.Context(), testAudience, ""); return err },
		"long action":         func() error { _, err := c.Token(t.Context(), testAudience, strings.Repeat("a", 129)); return err },
		"too many actions":    func() error { _, err := c.Token(t.Context(), testAudience, make65()...); return err },
		"round tripper scope": func() error { _, err := c.Transport(&fakeExchange{}, testTarget, testAudience, "a b"); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if call() == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func make65() []string {
	out := make([]string, 65)
	for i := range out {
		out[i] = "a" + strings.Repeat("b", i)
	}
	return out
}

// A dependency may replace http.DefaultTransport with a RoundTripper of its own; it is still the process default (and may
// still honour the environment proxy), so it is refused by identity, not only as an *http.Transport.
func TestNewRefusesAReplacedDefaultTransport(t *testing.T) {
	replaced := &fakeExchange{answer: issueEach(300)}
	previous := http.DefaultTransport
	http.DefaultTransport = replaced
	t.Cleanup(func() { http.DefaultTransport = previous })
	if _, err := New(Config{ExchangeURL: testBase, Issuer: testIssuerURL, Subject: KubernetesTokenFile(writeSubject(t, testSubject)), Transport: replaced}); err == nil {
		t.Fatal("New accepted the replaced http.DefaultTransport")
	}
}

// Every config field New refuses comes back as a *ConfigFieldError that names the Config field, so a caller can
// tell its own setting that is wrong without parsing text. The text of the cause stays as it was.
func TestNewNamesTheRejectedConfigField(t *testing.T) {
	path := writeSubject(t, testSubject)
	base := func() Config {
		return Config{ExchangeURL: testBase, Issuer: testIssuerURL, Subject: KubernetesTokenFile(path), Transport: &fakeExchange{}}
	}
	for name, tc := range map[string]struct {
		mutate func(*Config)
		field  ConfigField
	}{
		"no subject":                   {func(c *Config) { c.Subject = Subject{} }, FieldSubject},
		"empty subject path":           {func(c *Config) { c.Subject = KubernetesTokenFile("") }, FieldSubject},
		"nil transport":                {func(c *Config) { c.Transport = nil }, FieldTransport},
		"default transport":            {func(c *Config) { c.Transport = http.DefaultTransport }, FieldTransport},
		"proxied transport":            {func(c *Config) { c.Transport = &http.Transport{Proxy: http.ProxyFromEnvironment} }, FieldTransport},
		"no issuer":                    {func(c *Config) { c.Issuer = "" }, FieldIssuer},
		"issuer http":                  {func(c *Config) { c.Issuer = "http://auth-service.test" }, FieldIssuer},
		"issuer user info":             {func(c *Config) { c.Issuer = "https://u:p@auth-service.test" }, FieldIssuer},
		"issuer query":                 {func(c *Config) { c.Issuer = testIssuerURL + "?x" }, FieldIssuer},
		"issuer fragment":              {func(c *Config) { c.Issuer = testIssuerURL + "#x" }, FieldIssuer},
		"issuer upper case":            {func(c *Config) { c.Issuer = "https://Auth-Service.test" }, FieldIssuer},
		"no URL":                       {func(c *Config) { c.ExchangeURL = "" }, FieldExchangeURL},
		"URL user info":                {func(c *Config) { c.ExchangeURL = "https://u:p@auth-service" }, FieldExchangeURL},
		"URL path":                     {func(c *Config) { c.ExchangeURL = "https://auth-service/internal" }, FieldExchangeURL},
		"http without opt-in":          {func(c *Config) { c.ExchangeURL = "http://auth-service:8095" }, FieldExchangeURL},
		"timeout too short":            {func(c *Config) { c.Timeout = time.Millisecond }, FieldTimeout},
		"timeout too long":             {func(c *Config) { c.Timeout = MaxTimeout + time.Second }, FieldTimeout},
		"fraction 1":                   {func(c *Config) { c.RefreshFraction = 1 }, FieldRefreshFraction},
		"fraction negative":            {func(c *Config) { c.RefreshFraction = -0.1 }, FieldRefreshFraction},
		"margin negative":              {func(c *Config) { c.SafetyMargin = -time.Second }, FieldSafetyMargin},
		"margin too large":             {func(c *Config) { c.SafetyMargin = MaxSafetyMargin + time.Second }, FieldSafetyMargin},
		"floor negative":               {func(c *Config) { c.BackoffFloor = -time.Second }, FieldBackoffFloor},
		"floor above max":              {func(c *Config) { c.BackoffFloor = maxBackoff + time.Second }, FieldBackoffFloor},
		"floor above max, max ceiling": {func(c *Config) { c.BackoffFloor, c.BackoffCeiling = maxBackoff+time.Second, maxBackoff }, FieldBackoffFloor},
		"ceiling below floor":          {func(c *Config) { c.BackoffFloor, c.BackoffCeiling = 10*time.Second, time.Second }, FieldBackoffCeiling},
		"ceiling too long":             {func(c *Config) { c.BackoffCeiling = time.Hour }, FieldBackoffCeiling},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base()
			tc.mutate(&cfg)
			_, err := New(cfg)
			var field *ConfigFieldError
			if !errors.As(err, &field) {
				t.Fatalf("New returned %T %v, want a *ConfigFieldError", err, err)
			}
			if field.Field != tc.field {
				t.Fatalf("Field %q, want %q", field.Field, tc.field)
			}
			if field.Err == nil || !errors.Is(err, field.Err) || err.Error() != field.Err.Error() {
				t.Fatalf("Err %v: want the cause, unwrapped by errors.Is and used as the text, got text %q", field.Err, err)
			}
			for _, leak := range []string{"u:p", "Auth-Service"} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("error text holds a config value %q: %v", leak, err)
				}
			}
		})
	}
	if _, err := New(base()); err != nil {
		t.Fatalf("a good config: %v", err)
	}
}
