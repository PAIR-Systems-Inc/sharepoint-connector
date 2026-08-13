package config

import (
	"os"
	"path/filepath"
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

// TestSourceTokenIsCaseInsensitive: SOURCE is normalized (trimmed, lower-cased),
// and only the canonical "google-drive" spelling selects the Drive provider —
// there are no legacy aliases.
func TestSourceTokenIsCaseInsensitive(t *testing.T) {
	t.Setenv("GOODMEM_BASE_URL", "https://gm")
	t.Setenv("GOODMEM_API_KEY", "k")

	for _, v := range []string{"google-drive", "Google-Drive", "  GOOGLE-DRIVE  "} {
		t.Setenv("SOURCE", v)
		if cfg, _ := Load(""); cfg.Source != SourceGoogleDrive {
			t.Errorf("SOURCE=%q → Source %q, want %q", v, cfg.Source, SourceGoogleDrive)
		}
	}
	// The pre-rename spelling is no longer accepted.
	t.Setenv("SOURCE", "gdrive")
	t.Setenv("GOOGLE_DRIVE_ID", "0ABC")
	if cfg, _ := Load(""); cfg.ValidateSync() == nil {
		t.Error(`SOURCE="gdrive" should now be rejected as an unknown source`)
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

// Layering exists so a per-source file can hold only that source's credentials
// while shared settings live in one place. Precedence must be predictable:
// the real environment beats every file, and among files the earliest wins —
// which makes "most specific first" the natural way to order them.
func TestLoad_LayersEnvFilesLeftToRight(t *testing.T) {
	dir := t.TempDir()
	specific := filepath.Join(dir, ".env.smb")
	shared := filepath.Join(dir, ".env.shared")

	if err := os.WriteFile(specific, []byte(
		"SMB_HOST=fs-specific\nGOODMEM_BASE_URL=http://from-specific\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shared, []byte(
		"GOODMEM_BASE_URL=http://from-shared\nGOODMEM_API_KEY=key-from-shared\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, k := range []string{"SMB_HOST", "GOODMEM_BASE_URL", "GOODMEM_API_KEY", "SOURCE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}

	cfg, err := Load(specific, shared)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.SMBHost != "fs-specific" {
		t.Errorf("SMBHost = %q, want it from the specific file", cfg.SMBHost)
	}
	// Defined in both: the earlier (more specific) file must win.
	if cfg.GoodmemBaseURL != "http://from-specific" {
		t.Errorf("GoodmemBaseURL = %q, want the earlier file to win", cfg.GoodmemBaseURL)
	}
	// Defined only in the later file: still picked up.
	if cfg.GoodmemAPIKey != "key-from-shared" {
		t.Errorf("GoodmemAPIKey = %q, want it filled in from the shared file", cfg.GoodmemAPIKey)
	}
}

// A variable already present in the real environment must beat every file —
// that is what makes container/Fly secrets authoritative in production.
func TestLoad_RealEnvBeatsEveryFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, ".env")
	if err := os.WriteFile(f, []byte("GOODMEM_API_KEY=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOODMEM_API_KEY", "from-real-env")

	cfg, err := Load(f)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GoodmemAPIKey != "from-real-env" {
		t.Errorf("GoodmemAPIKey = %q, want the real environment to win", cfg.GoodmemAPIKey)
	}
}
