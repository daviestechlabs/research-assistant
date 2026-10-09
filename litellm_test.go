package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func gatewayEnvironment(t *testing.T, endpoint string) {
	t.Helper()
	t.Setenv("RESEARCH_LIMIT_OWNER", "litellm")
	u, _ := url.Parse(endpoint)
	origin := "http://litellm.models.svc.cluster.local:4000"
	if u.Hostname() == "127.0.0.1" {
		origin = u.Scheme + "://" + u.Host
	}
	t.Setenv("RESEARCH_LITELLM_ORIGIN", origin)
	t.Setenv("RESEARCH_CHAT_URL", endpoint)
	t.Setenv("RESEARCH_CHAT_MODEL", "fast-local")
	t.Setenv("RESEARCH_MODEL_KEY", "private-test-service-key")
}

func TestGatewayRetainsExhaustedLedgerAndUsesScopedAuthorization(t *testing.T) {
	d := testDB(t)
	day := time.Now().UTC().Format("2006-01-02")
	previous := testInference(t, d)
	if _, err := previous.budget.run("INSERT INTO usage VALUES(?,'answer',20,300000)", day); err != nil {
		t.Fatal(err)
	}
	previous.close()
	calls := 0
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer private-test-service-key" || r.Header.Get("Cache-Control") != "no-cache" {
			t.Error("gateway credential or cache policy changed")
		}
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer gateway.Close()
	gatewayEnvironment(t, gateway.URL+"/v1/chat/completions")
	i, err := newInference(d)
	if err != nil {
		t.Fatal(err)
	}
	defer i.close()
	if i.dailyCalls != 0 || i.dailyUnits != 0 {
		t.Fatal("gateway mode advertises an application daily quota")
	}
	var out map[string]bool
	if err := i.post(context.Background(), i.answerURL, "answer", map[string]string{"model": i.model}, 100, &out); err != nil || !out["ok"] || calls != 1 {
		t.Fatalf("gateway call after old allowance: %v", err)
	}
	reranker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("chat gateway key leaked to reranker")
		}
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer reranker.Close()
	if err := i.post(context.Background(), reranker.URL, "rerank", map[string]string{}, 5, &out); err != nil {
		t.Fatal(err)
	}
	i.close()
	i.budget = nil
	restarted, err := newInference(d)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.close()
	v, err := restarted.stats()
	if err != nil || v.(map[string]any)["model_calls_today"] != 22 || v.(map[string]any)["reserved_units_today"] != 300105 {
		t.Fatal("usage history was reset or lost after restart")
	}
}

func TestGatewayRejectsDirectBackendAndMissingServiceKey(t *testing.T) {
	for _, url := range []string{
		"http://direct-engine.models.svc.cluster.local:8000/v1/chat/completions",
		"http://litellm.models.svc.cluster.local:4000/other",
		"https://example.com/v1/chat/completions",
		"http://litellm.models.svc.cluster.local:4000/v1/chat/completions?secret=bad",
	} {
		t.Run(url, func(t *testing.T) {
			gatewayEnvironment(t, url)
			if _, err := newInference(testDB(t)); err == nil {
				t.Fatal("gateway limits enabled on an unapproved endpoint")
			}
		})
	}
	gatewayEnvironment(t, "http://litellm.models.svc.cluster.local:4000/v1/chat/completions")
	t.Setenv("RESEARCH_MODEL_KEY", "")
	if _, err := newInference(testDB(t)); err == nil {
		t.Fatal("gateway mode accepted no service key")
	}
}

func TestGatewayRateLimitHasBoundedPrivateError(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte("private upstream details"))
	}))
	defer gateway.Close()
	gatewayEnvironment(t, gateway.URL+"/v1/chat/completions")
	i, err := newInference(testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer i.close()
	var out any
	err = i.post(context.Background(), i.answerURL, "answer", nil, 1, &out)
	if err == nil || !strings.Contains(err.Error(), "retry shortly") || strings.Contains(err.Error(), "private") {
		t.Fatal("gateway rate limit lost or private details exposed")
	}
}

func TestGatewayResearchAliasesRetainScopedLimits(t *testing.T) {
	for _, model := range []string{"fast-local", "smart-local", "planner", "hosted-fixture"} {
		t.Run(model, func(t *testing.T) {
			gatewayEnvironment(t, "http://litellm.models.svc.cluster.local:4000/v1/chat/completions")
			t.Setenv("RESEARCH_CHAT_MODEL", model)
			i, err := newInference(testDB(t))
			if model == "fast-local" || model == "smart-local" {
				if err != nil {
					t.Fatal(err)
				}
				defer i.close()
				expectedTimeout := 90 * time.Second
				if model == "smart-local" {
					expectedTimeout = 240 * time.Second
				}
				if i.dailyCalls != 0 || i.model != model || i.client.Timeout != expectedTimeout {
					t.Fatal("approved alias changed the model or limit owner")
				}
			} else if err == nil {
				i.close()
				t.Fatal("unreviewed model enabled gateway mode")
			}
		})
	}
}
