package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMergeCodexAccountToAuthFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "auth.json")
	seed := map[string]any{
		"OPENAI_API_KEY": "keep-me",
		"tokens": map[string]any{
			"other_field": "keep-token-field",
		},
	}
	data, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}

	account := &codexAccount{
		ID:           "acc1",
		AccessToken:  "at",
		RefreshToken: "rt",
		IDToken:      "it",
		AccountID:    "aid",
		UpdatedAt:    time.Date(2026, 4, 2, 1, 2, 3, 0, time.UTC),
	}
	if err := mergeCodexAccountToAuthFile(target, account); err != nil {
		t.Fatal(err)
	}

	merged, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(merged, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["OPENAI_API_KEY"] != "keep-me" {
		t.Fatalf("OPENAI_API_KEY=%v", parsed["OPENAI_API_KEY"])
	}
	tokens := parsed["tokens"].(map[string]any)
	if tokens["access_token"] != "at" || tokens["refresh_token"] != "rt" || tokens["id_token"] != "it" || tokens["account_id"] != "aid" {
		t.Fatalf("tokens=%v", tokens)
	}
	if tokens["other_field"] != "keep-token-field" {
		t.Fatalf("tokens=%v", tokens)
	}
	if parsed["last_refresh"] != "2026-04-02T01:02:03Z" {
		t.Fatalf("last_refresh=%v", parsed["last_refresh"])
	}
}

func TestSelectCodexProvider(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{
			{Name: "x", Type: "openai-compatible"},
			{Name: "codex-a", Type: "codex"},
		},
	}
	provider, err := selectCodexProvider(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if provider.Name != "codex-a" {
		t.Fatalf("provider=%s", provider.Name)
	}
}

func TestSelectCodexAccountForCheckout(t *testing.T) {
	pool := &codexAccountPool{
		accounts: map[string]*codexAccount{
			"a": {ID: "a"},
			"b": {ID: "b"},
		},
		order: []string{"a", "b"},
	}
	account, err := selectCodexAccountForCheckout(pool, "b")
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "b" {
		t.Fatalf("account=%s", account.ID)
	}
}
