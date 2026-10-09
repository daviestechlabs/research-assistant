package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBrandingEscapesConfigurationAndKeepsManifestValid(t *testing.T) {
	t.Setenv("RESEARCH_DESK_NAME", `Desk </script><script>alert(1)</script>`)
	t.Setenv("RESEARCH_DESK_DESCRIPTION", `<img src=x onerror=alert(1)>`)
	t.Setenv("RESEARCH_DESK_ICON_ALT", `" onload="alert(1)`)
	body := string(brandedPage())
	if strings.Contains(body, "{{DESK_") || strings.Contains(body, `<script>alert(1)</script>`) || strings.Contains(body, `<img src=x`) || strings.Contains(body, `alt="" onload=`) {
		t.Fatal("branding injected markup")
	}
	var manifest map[string]any
	if err := json.Unmarshal(brandedManifest(), &manifest); err != nil || manifest["name"] != deskName() {
		t.Fatal("manifest is invalid", err)
	}
}
func TestGatewayTrustRequiresExactConfiguredOrigin(t *testing.T) {
	t.Setenv("RESEARCH_LIMIT_OWNER", "litellm")
	t.Setenv("RESEARCH_LITELLM_ORIGIN", "https://gateway.example.test")
	t.Setenv("RESEARCH_CHAT_URL", "https://gateway.example.test/v1/chat/completions")
	t.Setenv("RESEARCH_CHAT_MODEL", "smart-local")
	t.Setenv("RESEARCH_MODEL_KEY", "synthetic-key")
	i, err := newInference(testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	i.close()
	t.Setenv("RESEARCH_CHAT_URL", "https://other.example.test/v1/chat/completions")
	if _, err = newInference(testDB(t)); err == nil {
		t.Fatal("credential destination changed")
	}
}
func TestBooksRequireReviewedManifestAuthority(t *testing.T) {
	t.Setenv("RESEARCH_BOOK_MANIFEST", "/no/such/manifest.json")
	if _, err := approvedBooks(); err == nil {
		t.Fatal("unavailable manifest admitted")
	}
}
