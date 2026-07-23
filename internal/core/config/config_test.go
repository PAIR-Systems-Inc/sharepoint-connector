package config

import (
	"strings"
	"testing"
)

// TestValidateSyncBySource checks that required-config validation branches on
// SOURCE: gdrive needs GDRIVE_*, sharepoint needs Azure/SharePoint, Goodmem always.
func TestValidateSyncBySource(t *testing.T) {
	// Goodmem is always required.
	goodmem := func() {
		t.Setenv("GOODMEM_BASE_URL", "https://gm")
		t.Setenv("GOODMEM_API_KEY", "k")
	}

	// Default source is sharepoint, and it needs Azure + SharePoint.
	goodmem()
	if cfg, _ := Load(""); cfg.Source != "sharepoint" {
		t.Errorf("default Source = %q, want sharepoint", cfg.Source)
	}
	if cfg, _ := Load(""); cfg.ValidateSync() == nil {
		t.Error("sharepoint without Azure should fail validation")
	}
	t.Setenv("AZURE_AD_CLIENT_ID", "c")
	t.Setenv("AZURE_AD_TENANT_ID", "t")
	t.Setenv("AZURE_AD_CLIENT_SECRET", "s")
	t.Setenv("SHAREPOINT_SITE_URL", "https://x.sharepoint.com/sites/S")
	if cfg, _ := Load(""); cfg.ValidateSync() != nil {
		t.Errorf("sharepoint with full config should validate: %v", cfg.ValidateSync())
	}

	// gdrive needs GDRIVE_DRIVE_ID (not Azure). The service-account key is optional
	// — without it the source falls back to Application Default Credentials.
	t.Setenv("SOURCE", "gdrive")
	cfg, _ := Load("")
	if err := cfg.ValidateSync(); err == nil || !strings.Contains(err.Error(), "GDRIVE_DRIVE_ID") {
		t.Errorf("gdrive without drive id should fail on GDRIVE_DRIVE_ID, got: %v", err)
	}
	t.Setenv("GDRIVE_DRIVE_ID", "0ABC")
	if cfg, _ := Load(""); cfg.ValidateSync() != nil {
		t.Errorf("gdrive with just the drive id should validate (ADC fallback): %v", cfg.ValidateSync())
	}
	if cfg, _ := Load(""); cfg.HasServiceAccount() {
		t.Error("HasServiceAccount should be false with no GDRIVE_SA_JSON")
	}
	t.Setenv("GDRIVE_SA_JSON", `{"client_email":"x","private_key":"y"}`)
	if cfg, _ := Load(""); !cfg.HasServiceAccount() {
		t.Error("HasServiceAccount should be true with GDRIVE_SA_JSON set")
	}

	// An unknown source is rejected.
	t.Setenv("SOURCE", "dropbox")
	if cfg, _ := Load(""); cfg.ValidateSync() == nil {
		t.Error("unknown SOURCE should fail validation")
	}
}

// TestSpaceEmbedderAliases verifies the Python env-alias chains
// (GOODMEM_SPACE_ID / SPACE_ID / DEFAULT_SPACE_ID, and the embedder equivalents)
// are honored with GOODMEM_-prefixed names taking precedence.
func TestSpaceEmbedderAliases(t *testing.T) {
	// Alias-only (no GOODMEM_ prefix) must be picked up.
	t.Setenv("SPACE_ID", "space-from-alias")
	t.Setenv("DEFAULT_EMBEDDER_ID", "embedder-from-default")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GoodmemSpaceID != "space-from-alias" {
		t.Errorf("SpaceID = %q, want %q", cfg.GoodmemSpaceID, "space-from-alias")
	}
	if cfg.GoodmemEmbedderID != "embedder-from-default" {
		t.Errorf("EmbedderID = %q, want %q", cfg.GoodmemEmbedderID, "embedder-from-default")
	}

	// GOODMEM_-prefixed names win over aliases.
	t.Setenv("GOODMEM_SPACE_ID", "space-primary")
	t.Setenv("GOODMEM_EMBEDDER_ID", "embedder-primary")
	cfg, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GoodmemSpaceID != "space-primary" {
		t.Errorf("SpaceID precedence = %q, want %q", cfg.GoodmemSpaceID, "space-primary")
	}
	if cfg.GoodmemEmbedderID != "embedder-primary" {
		t.Errorf("EmbedderID precedence = %q, want %q", cfg.GoodmemEmbedderID, "embedder-primary")
	}
}

func TestEnvTruthy(t *testing.T) {
	truthy := []string{"1", "true", "TRUE", "Yes", "on", " on "}
	falsy := []string{"", "0", "false", "no", "off", "nope"}
	for _, v := range truthy {
		t.Setenv("GOODMEM_EXTRACT_PAGE_IMAGES", v)
		if !envTruthy("GOODMEM_EXTRACT_PAGE_IMAGES") {
			t.Errorf("envTruthy(%q) = false, want true", v)
		}
	}
	for _, v := range falsy {
		t.Setenv("GOODMEM_EXTRACT_PAGE_IMAGES", v)
		if envTruthy("GOODMEM_EXTRACT_PAGE_IMAGES") {
			t.Errorf("envTruthy(%q) = true, want false", v)
		}
	}
}
