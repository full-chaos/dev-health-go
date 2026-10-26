package authverify

import (
	"testing"
)

func TestMalformedDiscoveryOrJWKSIsUnavailableNotUnknownKid(t *testing.T) {
	cases := []struct {
		name  string
		apply func(s *issuerServer)
	}{
		{"discovery not json", func(s *issuerServer) { s.discovery = func(string) []byte { return []byte("{not json") } }},
		{"discovery wrong issuer", func(s *issuerServer) {
			s.discovery = func(string) []byte { return discoveryDoc("https://other.example.test", nil) }
		}},
		{"jwks not json", func(s *issuerServer) { s.jwks = []byte("{not json") }},
		{"jwks key of the wrong type", func(s *issuerServer) { s.jwks = []byte(`{"keys":[{"kty":"RSA","kid":"wl-a"}]}`) }},
	}
	for _, class := range []string{"user", "workload"} {
		for _, tc := range cases {
			t.Run(class+"/"+tc.name, func(t *testing.T) {
				h := newOutageHarness(t, class)
				h.srv.set(tc.apply)
				wantUnavailable(t, tc.name, h.verify(t, h.known, "wl-a"))
			})
		}
	}
}
