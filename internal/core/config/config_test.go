package config

import "testing"

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
