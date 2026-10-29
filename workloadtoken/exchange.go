package workloadtoken

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// exchange sends one token-exchange request and returns the cache entry of
// its answer. The window starts at the clock reading taken before the request
// is sent, so the time on the wire only shortens it.
func (c *Client) exchange(ctx context.Context, audience string, actions []string) (*entry, error) {
	subject, err := c.subject.read()
	if err != nil {
		return nil, err
	}
	form := url.Values{
		"grant_type":         {GrantTypeTokenExchange},
		"subject_token":      {subject},
		"subject_token_type": {TokenTypeJWT},
		"audience":           {audience},
	}
	if len(actions) > 0 {
		form.Set("scope", strings.Join(actions, " "))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, unavailable("request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	start := c.now()
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return nil, unavailable("timeout")
		}
		return nil, unavailable("transport")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes+1))
	if err != nil {
		return nil, unavailable("read")
	}
	if len(body) > maxAnswerBytes {
		return nil, unavailable("answer too large")
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusBadRequest:
		return nil, refusal(resp.Header.Get("Content-Type"), body)
	default:
		return nil, unavailable("status " + strconv.Itoa(resp.StatusCode))
	}
	token, lifetime, err := parseAnswer(resp.Header.Get("Content-Type"), body)
	if err != nil {
		return nil, err
	}
	if lifetime <= c.margin {
		return nil, unavailable("lifetime inside the safety margin")
	}
	notBefore, expires, err := tokenClaims(token, audience)
	if err != nil {
		return nil, err
	}
	// The strictest legal verifier (leeway 0, this host's clock) refuses a
	// token before its nbf: the issuer's clock is ahead. Refused at once.
	if now := c.now(); now.Before(notBefore) {
		c.logger.Warn("workload token not yet valid: the issuer clock is ahead of this host",
			"audience", audience, "ahead_seconds", int64((notBefore.Sub(now)+time.Second-1)/time.Second))
		return nil, unavailable("token not yet valid")
	}
	usableUntil := start.Add(lifetime)
	if expires.Before(usableUntil) {
		usableUntil = expires
	}
	usableUntil = usableUntil.Add(-c.margin)
	refreshAt := start.Add(time.Duration(float64(lifetime) * c.refreshFraction))
	if refreshAt.After(usableUntil) {
		refreshAt = usableUntil
	}
	fresh := &entry{token: token, notBefore: notBefore, refreshAt: refreshAt, usableUntil: usableUntil}
	if !fresh.usableAt(c.now()) {
		return nil, unavailable("answer arrived outside its usable window")
	}
	return fresh, nil
}

// The workload-token.v1 claim shapes its verifier enforces (authverify
// workload_token.go and accesstoken_wire.go); the client checks every one it
// can know before it caches or sends a token. The iss VALUE is the verifier's
// configuration, so only its shape is checked here.
var (
	claimMembers   = []string{"iss", "aud", "sub", "principal_type", "jti", "iat", "nbf", "exp"}
	issuerShape    = regexp.MustCompile(`^https://[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:[0-9]{1,5})?(/[A-Za-z0-9_~-][A-Za-z0-9._~-]{0,63}){0,8}$`)
	subjectShape   = regexp.MustCompile(`^prn_[A-Za-z0-9_-]+$`)
	tokenIDShape   = regexp.MustCompile(`^[A-Za-z0-9_-]{22,64}$`)
	errClaimsShape = unavailable("token claims break the workload-token contract")
)

// tokenClaims reads the payload of a workload token (no signature check: the
// client is not the verifier) and checks it as the verifier will: exactly the
// workload-token.v1 claims, each once; aud the requested audience (one string);
// principal_type workload; iss, sub and jti in their shapes; integer
// NumericDates with iat <= nbf < exp and exp - iat <= 300. It returns nbf and
// exp.
func tokenClaims(token, audience string) (time.Time, time.Time, error) {
	parts := strings.Split(token, ".")
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return time.Time{}, time.Time{}, unavailable("token payload is not base64url")
	}
	members, ok := uniqueObject(payload)
	if !ok || len(members) != len(claimMembers) {
		return time.Time{}, time.Time{}, errClaimsShape
	}
	for _, name := range claimMembers {
		if _, ok := members[name]; !ok {
			return time.Time{}, time.Time{}, errClaimsShape
		}
	}
	text := func(name string, shape *regexp.Regexp, want string) bool {
		var value string
		if json.Unmarshal(members[name], &value) != nil {
			return false
		}
		if shape != nil {
			return shape.MatchString(value)
		}
		return value == want
	}
	if !text("aud", nil, audience) || !text("principal_type", nil, "workload") || !text("iss", issuerShape, "") ||
		!text("sub", subjectShape, "") || !text("jti", tokenIDShape, "") {
		return time.Time{}, time.Time{}, errClaimsShape
	}
	var dates [3]int64
	for i, name := range []string{"iat", "nbf", "exp"} {
		value, err := strconv.ParseInt(string(bytes.TrimSpace(members[name])), 10, 64)
		if err != nil {
			return time.Time{}, time.Time{}, unavailable("token " + name + " is not an integer")
		}
		dates[i] = value
	}
	iat, nbf, exp := dates[0], dates[1], dates[2]
	if nbf < iat || exp <= nbf || exp-iat > int64(MaxLifetime/time.Second) {
		return time.Time{}, time.Time{}, unavailable("token dates break the workload-token contract")
	}
	return time.Unix(nbf, 0), time.Unix(exp, 0), nil
}

// uniqueObject decodes a JSON object whose member names are all distinct.
func uniqueObject(raw []byte) (map[string]json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if open, err := decoder.Token(); err != nil || open != json.Delim('{') {
		return nil, false
	}
	members := map[string]json.RawMessage{}
	for decoder.More() {
		key, err := decoder.Token()
		name, isName := key.(string)
		if err != nil || !isName {
			return nil, false
		}
		if _, seen := members[name]; seen {
			return nil, false
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, false
		}
		members[name] = value
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return nil, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return members, true
}

func unavailable(class string) error {
	return fmt.Errorf("%w (%s)", ErrExchangeUnavailable, class)
}

func isTimeout(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

func isJSON(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "application/json"
}

// refusal maps a 400 answer: invalid_grant and invalid_request are the two
// refusals the exchange answers; any other 400 is not its answer.
func refusal(contentType string, body []byte) error {
	if !isJSON(contentType) {
		return unavailable("400 without a JSON answer")
	}
	var answer struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &answer) != nil {
		return unavailable("400 with a malformed answer")
	}
	switch answer.Error {
	case "invalid_grant":
		return ErrExchangeRefused
	case "invalid_request":
		return ErrExchangeInvalidRequest
	default:
		return unavailable("400 with an unknown error code")
	}
}

// parseAnswer checks the 200 answer: the four RFC 8693 members, a Bearer
// token type, a JWT issued token type, an integer expires_in in 1..300 and a
// compact JWS access token. Unknown members are ignored.
func parseAnswer(contentType string, body []byte) (string, time.Duration, error) {
	if !isJSON(contentType) {
		return "", 0, unavailable("answer is not JSON")
	}
	var members map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&members); err != nil || members == nil {
		return "", 0, unavailable("malformed answer")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return "", 0, unavailable("malformed answer")
	}
	var accessToken, tokenType, issuedType string
	for name, target := range map[string]*string{"access_token": &accessToken, "token_type": &tokenType, "issued_token_type": &issuedType} {
		raw, ok := members[name]
		if !ok || json.Unmarshal(raw, target) != nil {
			return "", 0, unavailable("answer lacks " + name)
		}
	}
	if !strings.EqualFold(tokenType, "Bearer") {
		return "", 0, unavailable("token_type is not Bearer")
	}
	if issuedType != TokenTypeJWT {
		return "", 0, unavailable("issued_token_type is not a JWT")
	}
	if len(accessToken) > maxAccessTokenBytes || !workloadTokenShape(accessToken) {
		return "", 0, unavailable("access_token is not a compact JWS")
	}
	raw, ok := members["expires_in"]
	if !ok {
		return "", 0, unavailable("answer lacks expires_in")
	}
	seconds, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
	if err != nil || seconds < 1 || seconds > int64(MaxLifetime/time.Second) {
		return "", 0, unavailable("expires_in is not an integer in 1..300")
	}
	return accessToken, time.Duration(seconds) * time.Second, nil
}

// compactJWS is the shape check of a compact JWS of any algorithm (the
// subject): three segments, each non-empty unpadded base64url.
func compactJWS(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
				return false
			}
		}
		if _, err := base64.RawURLEncoding.DecodeString(part); err != nil {
			return false
		}
	}
	return true
}

// workloadTokenShape is the shape of a workload-token.v1 access token as its
// verifier requires it, short of the signature check: a compact JWS whose
// header is a JSON object with alg EdDSA, typ workload+jwt and a kid, and
// whose signature has the Ed25519 length.
func workloadTokenShape(token string) bool {
	if !compactJWS(token) {
		return false
	}
	parts := strings.Split(token, ".")
	enc := base64.RawURLEncoding.Strict()
	rawHeader, err := enc.DecodeString(parts[0])
	if err != nil {
		return false
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		Kid string `json:"kid"`
	}
	if json.Unmarshal(rawHeader, &header) != nil || header.Alg != "EdDSA" || header.Typ != "workload+jwt" || header.Kid == "" {
		return false
	}
	signature, err := enc.DecodeString(parts[2])
	return err == nil && len(signature) == ed25519.SignatureSize
}

// read returns the subject token. It is read for every exchange and never
// kept: a rotated file is used at once.
func (s Subject) read() (string, error) {
	switch s.kind {
	case subjectKubernetes:
		return readKubernetesToken(s.path)
	default:
		return "", ErrSubjectUnreadable
	}
}

func readKubernetesToken(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%w (open)", ErrSubjectUnreadable)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w (not a regular file)", ErrSubjectUnreadable)
	}
	// Group or other may not write it: whoever can write it can swap the
	// credential. Read bits are allowed (secret mounts are 0444).
	if info.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("%w (writable by group or other)", ErrSubjectUnreadable)
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxSubjectBytes+2))
	if err != nil {
		return "", fmt.Errorf("%w (read)", ErrSubjectUnreadable)
	}
	value := strings.TrimSuffix(string(raw), "\n")
	if value == "" || len(value) > maxSubjectBytes {
		return "", fmt.Errorf("%w (size)", ErrSubjectUnreadable)
	}
	if !compactJWS(value) {
		return "", fmt.Errorf("%w (shape)", ErrSubjectUnreadable)
	}
	return value, nil
}
