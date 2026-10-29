package workloadtoken

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func answerWith(status int, contentType, body string) func(int, url.Values) (*http.Response, error) {
	return func(int, url.Values) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}
}

func TestTheAnswerIsCheckedBeforeATokenIsCached(t *testing.T) {
	good := mintToken(7, 300)
	const jt = `"issued_token_type":"urn:ietf:params:oauth:token-type:jwt"`
	accept := map[string]string{
		"canonical":       `{"access_token":"` + good + `",` + jt + `,"token_type":"Bearer","expires_in":300}`,
		"bearer any case": `{"access_token":"` + good + `",` + jt + `,"token_type":"bearer","expires_in":300}`,
		"unknown member":  `{"access_token":"` + good + `",` + jt + `,"token_type":"Bearer","expires_in":31,"x":1}`,
	}
	for name, body := range accept {
		t.Run("accept "+name, func(t *testing.T) {
			c := newTestClient(t, &fakeExchange{answer: answerWith(200, "application/json; charset=utf-8", body)}, writeSubject(t, testSubject))
			if token, err := c.Token(context.Background(), testAudience); err != nil || token != good {
				t.Fatalf("token %q err %v", token, err)
			}
		})
	}
	member := func(accessToken, tokenType, issuedType, expiresIn string) string {
		var parts []string
		if accessToken != "-" {
			parts = append(parts, `"access_token":`+accessToken)
		}
		if tokenType != "-" {
			parts = append(parts, `"token_type":`+tokenType)
		}
		if issuedType != "-" {
			parts = append(parts, `"issued_token_type":`+issuedType)
		}
		if expiresIn != "-" {
			parts = append(parts, `"expires_in":`+expiresIn)
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	q := func(s string) string { return `"` + s + `"` }
	gt, bt, it, e := q(good), q("Bearer"), q(TokenTypeJWT), "300"
	refuse := map[string]struct {
		status      int
		contentType string
		body        string
		class       error
	}{
		"no access_token":       {200, "application/json", member("-", bt, it, e), ErrExchangeUnavailable},
		"empty access_token":    {200, "application/json", member(q(""), bt, it, e), ErrExchangeUnavailable},
		"access_token number":   {200, "application/json", member("1", bt, it, e), ErrExchangeUnavailable},
		"two segments":          {200, "application/json", member(q("eyJh.cGF5"), bt, it, e), ErrExchangeUnavailable},
		"four segments":         {200, "application/json", member(q(good+".x"), bt, it, e), ErrExchangeUnavailable},
		"bad character":         {200, "application/json", member(q("eyJh.cG+F5.c2ln"), bt, it, e), ErrExchangeUnavailable},
		"empty payload":         {200, "application/json", member(q("eyJh..c2ln"), bt, it, e), ErrExchangeUnavailable},
		"token too large":       {200, "application/json", member(q("eyJh."+strings.Repeat("a", maxAccessTokenBytes)+".c2ln"), bt, it, e), ErrExchangeUnavailable},
		"no token_type":         {200, "application/json", member(gt, "-", it, e), ErrExchangeUnavailable},
		"token_type MAC":        {200, "application/json", member(gt, q("MAC"), it, e), ErrExchangeUnavailable},
		"no issued_token_type":  {200, "application/json", member(gt, bt, "-", e), ErrExchangeUnavailable},
		"access token type":     {200, "application/json", member(gt, bt, q("urn:ietf:params:oauth:token-type:access_token"), e), ErrExchangeUnavailable},
		"no expires_in":         {200, "application/json", member(gt, bt, it, "-"), ErrExchangeUnavailable},
		"expires_in 0":          {200, "application/json", member(gt, bt, it, "0"), ErrExchangeUnavailable},
		"expires_in -1":         {200, "application/json", member(gt, bt, it, "-1"), ErrExchangeUnavailable},
		"expires_in 301":        {200, "application/json", member(gt, bt, it, "301"), ErrExchangeUnavailable},
		"expires_in 1.5":        {200, "application/json", member(gt, bt, it, "1.5"), ErrExchangeUnavailable},
		"expires_in 3e2":        {200, "application/json", member(gt, bt, it, "3e2"), ErrExchangeUnavailable},
		"expires_in string":     {200, "application/json", member(gt, bt, it, q("300")), ErrExchangeUnavailable},
		"expires_in null":       {200, "application/json", member(gt, bt, it, "null"), ErrExchangeUnavailable},
		"inside safety margin":  {200, "application/json", member(gt, bt, it, "30"), ErrExchangeUnavailable},
		"not JSON type":         {200, "text/plain", member(gt, bt, it, e), ErrExchangeUnavailable},
		"no content type":       {200, "", member(gt, bt, it, e), ErrExchangeUnavailable},
		"not an object":         {200, "application/json", `[1]`, ErrExchangeUnavailable},
		"null":                  {200, "application/json", `null`, ErrExchangeUnavailable},
		"trailing data":         {200, "application/json", member(gt, bt, it, e) + `{}`, ErrExchangeUnavailable},
		"truncated":             {200, "application/json", `{"access_token":`, ErrExchangeUnavailable},
		"answer too large":      {200, "application/json", `{"pad":"` + strings.Repeat("a", maxAnswerBytes) + `"}`, ErrExchangeUnavailable},
		"400 invalid_grant":     {400, "application/json", `{"error":"invalid_grant"}`, ErrExchangeRefused},
		"400 invalid_request":   {400, "application/json", `{"error":"invalid_request"}`, ErrExchangeInvalidRequest},
		"400 other code":        {400, "application/json", `{"error":"unsupported_grant_type"}`, ErrExchangeUnavailable},
		"400 not JSON":          {400, "text/html", `<html>`, ErrExchangeUnavailable},
		"400 malformed":         {400, "application/json", `{`, ErrExchangeUnavailable},
		"401":                   {401, "application/json", `{}`, ErrExchangeUnavailable},
		"404":                   {404, "application/json", `{}`, ErrExchangeUnavailable},
		"500":                   {500, "application/json", `{}`, ErrExchangeUnavailable},
		"503":                   {503, "application/json", `{"error":"temporarily_unavailable"}`, ErrExchangeUnavailable},
		"200 with a 400 answer": {200, "application/json", `{"error":"invalid_grant"}`, ErrExchangeUnavailable},
	}
	for name, tc := range refuse {
		t.Run("refuse "+name, func(t *testing.T) {
			c := newTestClient(t, &fakeExchange{answer: answerWith(tc.status, tc.contentType, tc.body)}, writeSubject(t, testSubject))
			token, err := c.Token(context.Background(), testAudience)
			if token != "" {
				t.Fatalf("a token was returned for a refused answer")
			}
			mustNoUsable(t, err, tc.class)
			if strings.Contains(err.Error(), good) || strings.Contains(err.Error(), testSubject) {
				t.Fatalf("the error carries a credential: %v", err)
			}
		})
	}
}

func TestARedirectIsNeverFollowed(t *testing.T) {
	exchange := &fakeExchange{answer: func(n int, _ url.Values) (*http.Response, error) {
		if n == 0 {
			return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{"Location": {"https://elsewhere.test/collect"}},
				Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return issued(mintToken(n, 300), 300), nil
	}}
	c := newTestClient(t, exchange, writeSubject(t, testSubject))
	_, err := c.Token(context.Background(), testAudience)
	mustNoUsable(t, err, ErrExchangeUnavailable)
	if got := exchange.count(); got != 1 {
		t.Fatalf("%d requests, want 1: the redirect target received the subject token", got)
	}
}

func TestNoErrorTextCarriesACredential(t *testing.T) {
	secretSubject := "eyJzZWNyZXQ.U0VDUkVUU1VCSkVDVA.c2VjcmV0"
	secretToken := "eyJzZWNyZXQ.U0VDUkVUVE9LRU4.c2VjcmV0"
	notJWS := "SECRETVALUENOTAJWS"
	cases := map[string]struct {
		subject string
		answer  func(int, url.Values) (*http.Response, error)
	}{
		"refused":         {secretSubject, answerWith(400, "application/json", `{"error":"invalid_grant"}`)},
		"bad expires_in":  {secretSubject, answerWith(200, "application/json", `{"access_token":"`+secretToken+`","token_type":"Bearer","issued_token_type":"`+TokenTypeJWT+`","expires_in":0}`)},
		"bad token_type":  {secretSubject, answerWith(200, "application/json", `{"access_token":"`+secretToken+`","token_type":"`+secretToken+`","issued_token_type":"`+TokenTypeJWT+`","expires_in":300}`)},
		"transport error": {secretSubject, func(int, url.Values) (*http.Response, error) { return nil, errors.New("echo " + secretSubject) }},
		"subject shape":   {notJWS, issueEach(300)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, &fakeExchange{answer: tc.answer}, writeSubject(t, tc.subject))
			_, err := c.Token(context.Background(), testAudience)
			if err == nil {
				t.Fatal("want an error")
			}
			for _, secret := range []string{secretSubject, secretToken, notJWS, "U0VDUkVU"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("the error %q carries %q", err, secret)
				}
			}
		})
	}
}
