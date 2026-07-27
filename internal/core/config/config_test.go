package config

import (
	"strings"
	"testing"
)

// TestValidateSyncBySource checks that required-config validation branches on
// SOURCE: google-drive needs GOOGLE_DRIVE_*, sharepoint needs Azure/SharePoint,
// Goodmem always.
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

	// google-drive needs GOOGLE_DRIVE_ID (not Azure). The service-account key is
	// optional — without it the source falls back to Application Default Credentials.
	t.Setenv("SOURCE", "google-drive")
	cfg, _ := Load("")
	if err := cfg.ValidateSync(); err == nil || !strings.Contains(err.Error(), "GOOGLE_DRIVE_ID") {
		t.Errorf("google-drive without drive id should fail on GOOGLE_DRIVE_ID, got: %v", err)
	}
	t.Setenv("GOOGLE_DRIVE_ID", "0ABC")
	if cfg, _ := Load(""); cfg.ValidateSync() != nil {
		t.Errorf("google-drive with just the drive id should validate (ADC fallback): %v", cfg.ValidateSync())
	}
	if cfg, _ := Load(""); cfg.HasServiceAccount() {
		t.Error("HasServiceAccount should be false with no GOOGLE_DRIVE_SA_JSON")
	}
	t.Setenv("GOOGLE_DRIVE_SA_JSON", `{"client_email":"x","private_key":"y"}`)
	if cfg, _ := Load(""); !cfg.HasServiceAccount() {
		t.Error("HasServiceAccount should be true with GOOGLE_DRIVE_SA_JSON set")
	}

	// An unknown source is rejected.
	t.Setenv("SOURCE", "dropbox")
	if cfg, _ := Load(""); cfg.ValidateSync() == nil {
		t.Error("unknown SOURCE should fail validation")
	}
}

// TestGoogleDriveDeprecatedAliases: the pre-rename spellings must keep working so
// existing .env files don't break — SOURCE=gdrive normalizes to "google-drive",
// and the GDRIVE_* variables are read when the GOOGLE_DRIVE_* ones are unset.
func TestGoogleDriveDeprecatedAliases(t *testing.T) {
	t.Setenv("GOODMEM_BASE_URL", "https://gm")
	t.Setenv("GOODMEM_API_KEY", "k")

	// Legacy source spellings all normalize to the canonical token.
	for _, legacy := range []string{"gdrive", "GDrive", "googledrive", "google_drive"} {
		t.Setenv("SOURCE", legacy)
		if cfg, _ := Load(""); cfg.Source != SourceGoogleDrive {
			t.Errorf("SOURCE=%q → Source %q, want %q", legacy, cfg.Source, SourceGoogleDrive)
		}
	}

	// Legacy variable names still populate the config and satisfy validation.
	t.Setenv("SOURCE", "gdrive")
	t.Setenv("GDRIVE_DRIVE_ID", "0LEGACY")
	t.Setenv("GDRIVE_SA_JSON", `{"client_email":"x","private_key":"y"}`)
	cfg, _ := Load("")
	if cfg.GoogleDriveID != "0LEGACY" {
		t.Errorf("GDRIVE_DRIVE_ID alias not honored: got %q", cfg.GoogleDriveID)
	}
	if !cfg.HasServiceAccount() {
		t.Error("GDRIVE_SA_JSON alias not honored")
	}
	if err := cfg.ValidateSync(); err != nil {
		t.Errorf("legacy-only config should validate: %v", err)
	}

	// The canonical name wins when both are set.
	t.Setenv("GOOGLE_DRIVE_ID", "0CANON")
	if cfg, _ := Load(""); cfg.GoogleDriveID != "0CANON" {
		t.Errorf("GOOGLE_DRIVE_ID should win over the alias: got %q", cfg.GoogleDriveID)
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
