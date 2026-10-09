package main

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// The gateway owns OAuth sessions. The app independently verifies its signed
// ID token and permitted subject. No client secret enters this process.
type tokenVerifier struct {
	issuer, audience, jwksURL string
	subjects                  map[string]bool
	client                    *http.Client
	mu                        sync.Mutex
	keys                      map[string]*rsa.PublicKey
	loaded                    time.Time
}

func publicConfiguration() (string, *tokenVerifier, error) {
	origin := os.Getenv("RESEARCH_PUBLIC_ORIGIN")
	access := os.Getenv("RESEARCH_PUBLIC_ACCESS")
	if access != "" && access != "false" && access != "true" {
		return "", nil, errors.New("RESEARCH_PUBLIC_ACCESS must be true or false")
	}
	issuer, audience, subjects := os.Getenv("RESEARCH_OIDC_ISSUER"), os.Getenv("RESEARCH_OIDC_AUDIENCE"), os.Getenv("RESEARCH_OIDC_SUBJECTS")
	if origin == "" && issuer == "" && audience == "" && subjects == "" && access != "true" {
		return "", nil, nil
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", nil, errors.New("public origin must be an HTTPS origin")
	}
	if access == "true" {
		if issuer != "" || audience != "" || subjects != "" || os.Getenv("RESEARCH_OIDC_ORIGIN_ADDRESS") != "" {
			return "", nil, errors.New("open hosting must omit OIDC configuration")
		}
		return origin, nil, nil
	}
	p, err := url.Parse(issuer)
	if err != nil || p.Scheme != "https" || p.Host == "" || p.User != nil || p.RawQuery != "" || p.Fragment != "" || !strings.HasSuffix(p.Path, "/") || audience == "" || subjects == "" {
		return "", nil, errors.New("public hosting requires an HTTPS OIDC issuer, audience, and allowed subjects")
	}
	v := &tokenVerifier{issuer: issuer, audience: audience, jwksURL: issuer + "jwks/", subjects: map[string]bool{}, client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if address := os.Getenv("RESEARCH_OIDC_ORIGIN_ADDRESS"); address != "" {
		if _, port, err := net.SplitHostPort(address); err != nil || port != "443" {
			return "", nil, errors.New("OIDC origin address must use port 443")
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, network, target string) (net.Conn, error) {
			if target != net.JoinHostPort(p.Hostname(), "443") {
				return nil, errors.New("unexpected OIDC destination")
			}
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, address)
		}
		// TLS still verifies the public issuer hostname and system trust chain.
		v.client.Transport = transport
	}
	for _, sub := range strings.Split(subjects, ",") {
		sub = strings.TrimSpace(sub)
		if sub == "" {
			return "", nil, errors.New("empty allowed OIDC subject")
		}
		v.subjects[sub] = true
	}
	return origin, v, nil
}

func (v *tokenVerifier) publicKey(ctx context.Context, kid string) *rsa.PublicKey {
	v.mu.Lock()
	defer v.mu.Unlock()
	if time.Since(v.loaded) < 5*time.Minute {
		return v.keys[kid]
	}
	// Failed refreshes are cached briefly to prevent request amplification.
	v.loaded = time.Now().Add(-4 * time.Minute)
	req, err := http.NewRequestWithContext(ctx, "GET", v.jwksURL, nil)
	if err != nil {
		return nil
	}
	r, err := v.client.Do(req)
	if err != nil {
		return nil
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 65537))
	if err != nil || len(body) > 65536 {
		return nil
	}
	var set struct {
		Keys []struct{ Kty, Kid, N, E, Alg, Use string }
	}
	if json.Unmarshal(body, &set) != nil || len(set.Keys) > 32 {
		return nil
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || (k.Alg != "" && k.Alg != "RS256") || (k.Use != "" && k.Use != "sig") || k.Kid == "" {
			continue
		}
		n, e1 := base64.RawURLEncoding.DecodeString(k.N)
		e, e2 := base64.RawURLEncoding.DecodeString(k.E)
		if e1 != nil || e2 != nil || len(n) < 256 || len(n) > 1024 || len(e) == 0 || len(e) > 4 {
			continue
		}
		ei := 0
		for _, b := range e {
			ei = ei*256 + int(b)
		}
		if ei < 3 || ei%2 == 0 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: ei}
	}
	v.keys = keys
	v.loaded = time.Now()
	return keys[kid]
}

func (v *tokenVerifier) authorized(ctx context.Context, token string) bool {
	_, ok := v.subject(ctx, token)
	return ok
}

func (v *tokenVerifier) subject(ctx context.Context, token string) (string, bool) {
	if len(token) == 0 || len(token) > 12288 {
		return "", false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false
	}
	header, e1 := base64.RawURLEncoding.DecodeString(parts[0])
	body, e2 := base64.RawURLEncoding.DecodeString(parts[1])
	signature, e3 := base64.RawURLEncoding.DecodeString(parts[2])
	if e1 != nil || e2 != nil || e3 != nil {
		return "", false
	}
	var h struct{ Alg, Kid string }
	if json.Unmarshal(header, &h) != nil || h.Alg != "RS256" || h.Kid == "" {
		return "", false
	}
	key := v.publicKey(ctx, h.Kid)
	if key == nil {
		return "", false
	}
	hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, hash[:], signature) != nil {
		return "", false
	}
	var c struct {
		Iss, Sub      string
		Aud           json.RawMessage
		Exp, Nbf, Iat int64
	}
	if json.Unmarshal(body, &c) != nil {
		return "", false
	}
	now := time.Now().Unix()
	if c.Iss != v.issuer || !v.subjects[c.Sub] || c.Exp <= now || c.Nbf > now+30 || c.Iat > now+30 {
		return "", false
	}
	var single string
	if json.Unmarshal(c.Aud, &single) == nil {
		return c.Sub, single == v.audience
	}
	var multiple []string
	if json.Unmarshal(c.Aud, &multiple) != nil {
		return "", false
	}
	for _, a := range multiple {
		if a == v.audience {
			return c.Sub, true
		}
	}
	return "", false
}
