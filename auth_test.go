package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPublicTokenValidation(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "RSA", "kid": "test", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB", "alg": "RS256"}}})
	}))
	defer server.Close()
	v := &tokenVerifier{issuer: "https://issuer/", audience: "desk", subjects: map[string]bool{"owner": true}, jwksURL: server.URL, client: server.Client()}
	sign := func(claims map[string]any) string {
		b, _ := json.Marshal(claims)
		h := base64.RawURLEncoding.EncodeToString([]byte("{\"alg\":\"RS256\",\"kid\":\"test\"}"))
		content := h + "." + base64.RawURLEncoding.EncodeToString(b)
		digest := sha256.Sum256([]byte(content))
		s, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
		return content + "." + base64.RawURLEncoding.EncodeToString(s)
	}
	base := map[string]any{"iss": v.issuer, "aud": v.audience, "sub": "owner", "exp": time.Now().Unix() + 60}
	token := sign(base)
	if !v.authorized(context.Background(), token) {
		t.Fatal("valid owner token denied")
	}
	for _, field := range []string{"iss", "aud", "sub", "exp", "nbf"} {
		claims := map[string]any{}
		for k, val := range base {
			claims[k] = val
		}
		switch field {
		case "exp":
			claims[field] = time.Now().Unix() - 1
		case "nbf":
			claims[field] = time.Now().Unix() + 600
		default:
			claims[field] = "other"
		}
		if v.authorized(context.Background(), sign(claims)) {
			t.Fatalf("accepted invalid %s", field)
		}
	}
	if v.authorized(context.Background(), token[:len(token)-10]+"xxxxxxxxxx") || v.authorized(context.Background(), "") || v.authorized(context.Background(), "eyJhbGciOiJub25lIn0.e30.") {
		t.Fatal("accepted forged token")
	}
	if requests != 1 {
		t.Fatalf("JWKS fetched %d times", requests)
	}
	d := testDB(t)
	i := testInference(t, d)
	h := securedHandler(d, i, "https://research.example", v)
	for _, path := range []string{"/", "/api/stats", "/manifest.webmanifest", "/graph.js", "/quality.js", "/api/quality"} {
		r := httptest.NewRequest("GET", "https://research.example"+path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("anonymous %s returned %d", path, w.Code)
		}
		r.Header.Set("x-envoy-oidc-id-token", token)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("owner %s returned %d", path, w.Code)
		}
		r.Header.Del("x-envoy-oidc-id-token")
		r.Header.Set("Authorization", "Bearer "+token)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("signed bearer %s returned %d", path, w.Code)
		}
		for _, invalid := range []string{"Bearer forged", "Basic " + token, "Bearer " + sign(map[string]any{"iss": v.issuer, "aud": "other", "sub": "owner", "exp": time.Now().Unix() + 60})} {
			r.Header.Set("Authorization", invalid)
			w = httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 401 {
				t.Fatalf("invalid bearer %s returned %d", path, w.Code)
			}
		}
	}
	for _, header := range []string{"", "invalid"} {
		r := httptest.NewRequest("POST", "https://research.example/api/graph", strings.NewReader(`{"query":"geology","ids":["arxiv:1706.03762"]}`))
		r.Header.Set("Origin", "https://research.example")
		r.Header.Set("x-envoy-oidc-id-token", header)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("unauthorized graph returned %d", w.Code)
		}
	}

}

func TestPartialPublicConfigurationFails(t *testing.T) {
	t.Setenv("RESEARCH_PUBLIC_ACCESS", "")
	t.Setenv("RESEARCH_PUBLIC_ORIGIN", "https://research.example")
	t.Setenv("RESEARCH_OIDC_ISSUER", "")
	t.Setenv("RESEARCH_OIDC_AUDIENCE", "")
	t.Setenv("RESEARCH_OIDC_SUBJECTS", "")
	if _, _, err := publicConfiguration(); err == nil {
		t.Fatal("incomplete public configuration accepted")
	}
}

func TestOpenPublicConfiguration(t *testing.T) {
	for _, name := range []string{"RESEARCH_OIDC_ISSUER", "RESEARCH_OIDC_AUDIENCE", "RESEARCH_OIDC_SUBJECTS", "RESEARCH_OIDC_ORIGIN_ADDRESS"} {
		t.Setenv(name, "")
	}
	t.Setenv("RESEARCH_PUBLIC_ACCESS", "true")
	t.Setenv("RESEARCH_PUBLIC_ORIGIN", "https://research.example")
	origin, verifier, err := publicConfiguration()
	if err != nil || origin != "https://research.example" || verifier != nil {
		t.Fatalf("open configuration: %q %v %v", origin, verifier, err)
	}
	for _, origin := range []string{"", "http://research.example", "https://research.example/path"} {
		t.Setenv("RESEARCH_PUBLIC_ORIGIN", origin)
		if _, _, err := publicConfiguration(); err == nil {
			t.Fatalf("accepted unsafe origin %q", origin)
		}
	}
	t.Setenv("RESEARCH_PUBLIC_ORIGIN", "https://research.example")
	t.Setenv("RESEARCH_PUBLIC_ACCESS", "yes")
	if _, _, err := publicConfiguration(); err == nil {
		t.Fatal("accepted ambiguous public access")
	}
	t.Setenv("RESEARCH_PUBLIC_ACCESS", "true")
	t.Setenv("RESEARCH_OIDC_SUBJECTS", "owner")
	if _, _, err := publicConfiguration(); err == nil {
		t.Fatal("accepted conflicting access modes")
	}
}

func TestOpenHTTPMetadata(t *testing.T) {
	d := testDB(t)
	loadFixture(t, d)
	h := securedHandler(d, testInference(t, d), "https://research.example", nil)
	for _, path := range []string{"/", "/api/stats", "/manifest.webmanifest", "/graph.js", "/quality.js", "/api/quality"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "https://research.example"+path, nil))
		if w.Code != 200 || w.Header().Get("Location") != "" {
			t.Fatalf("anonymous %s returned %d", path, w.Code)
		}
		if path == "/api/stats" && !strings.Contains(w.Body.String(), `"authentication_required":false`) {
			t.Fatal("open access missing from stats")
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "https://research.example/api/search", strings.NewReader(`{"query":"attention"}`))
	r.Header.Set("Origin", "https://research.example")
	h.ServeHTTP(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "nas_ref") || strings.Contains(w.Body.String(), "s3://") {
		t.Fatalf("public metadata response: %d %s", w.Code, w.Body.String())
	}
	var result SearchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Documents) == 0 || result.ModelCalls != 0 {
		t.Fatalf("keyword results: %v %+v", err, result)
	}
	docs, err := d.getDocuments([]string{"arxiv:1706.03762"})
	if err != nil || len(docs) != 1 || docs[0].NASRef == "" {
		t.Fatal("operator NAS reference was lost")
	}
	clean := browserDocuments(docs)
	if clean[0].NASRef != "" || docs[0].NASRef == "" {
		t.Fatal("browser redaction mutated source records")
	}
}
