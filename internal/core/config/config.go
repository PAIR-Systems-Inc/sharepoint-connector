// Package config loads connector configuration from the environment and, for
// local runs, an optional .env file. In production, secrets come from the
// process environment (Fly.io secrets); .env is only a local convenience.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Config holds runtime configuration. Field names mirror the .env variables.
type Config struct {
	// Source selects the content provider: "sharepoint" (default) or "gdrive".
	Source string

	// Azure AD / SharePoint — required when Source is "sharepoint".
	AzureClientID     string
	AzureTenantID     string
	AzureClientSecret string
	SharePointSiteURL string

	// Google Drive — required when Source is "gdrive".
	GDriveServiceAccount     string // inline service-account JSON (e.g. a Fly secret)
	GDriveServiceAccountFile string // ...or a path to the JSON key file
	GDriveDriveID            string // the Shared Drive id

	// Goodmem — required for a sync (unless the deploy provisions it).
	GoodmemBaseURL    string
	GoodmemAPIKey     string
	GoodmemSpaceID    string // set exactly one of SpaceID / EmbedderID
	GoodmemEmbedderID string

	// Graph webhook — event-triggered (serve) only.
	GraphClientState         string
	GraphNotificationURL     string
	GraphPort                string
	GraphSubscriptionMinutes string

	// Optional SharePoint scoping.
	SharePointSearchScope string
	SharePointFolderPath  string
	SharePointStartDate   string

	// OpenAI — used only to create a text-embedding-3-small embedder when a fresh
	// Goodmem has none (e.g. after a hands-free deploy).
	OpenAIAPIKey string

	// ExtractPageImages hints Goodmem to extract page images (e.g. PDF page
	// screenshots for citations). From GOODMEM_EXTRACT_PAGE_IMAGES.
	ExtractPageImages bool
}

// Load reads configuration from the process environment. If envFile is
// non-empty and exists, its KEY=VALUE lines are loaded first for any variable
// not already set in the environment (real env wins, matching Fly secrets).
func Load(envFile string) (*Config, error) {
	if envFile != "" {
		if err := loadDotEnv(envFile); err != nil {
			return nil, err
		}
	}
	return &Config{
		Source:                   sourceFromEnv(),
		AzureClientID:            os.Getenv("AZURE_AD_CLIENT_ID"),
		AzureTenantID:            os.Getenv("AZURE_AD_TENANT_ID"),
		AzureClientSecret:        os.Getenv("AZURE_AD_CLIENT_SECRET"),
		SharePointSiteURL:        os.Getenv("SHAREPOINT_SITE_URL"),
		GDriveServiceAccount:     os.Getenv("GDRIVE_SA_JSON"),
		GDriveServiceAccountFile: os.Getenv("GDRIVE_SA_JSON_FILE"),
		GDriveDriveID:            os.Getenv("GDRIVE_DRIVE_ID"),
		GoodmemBaseURL:           os.Getenv("GOODMEM_BASE_URL"),
		GoodmemAPIKey:            os.Getenv("GOODMEM_API_KEY"),
		GoodmemSpaceID:           firstEnv("GOODMEM_SPACE_ID", "SPACE_ID", "DEFAULT_SPACE_ID"),
		GoodmemEmbedderID:        firstEnv("GOODMEM_EMBEDDER_ID", "EMBEDDER_ID", "DEFAULT_EMBEDDER_ID"),
		GraphClientState:         os.Getenv("GRAPH_CLIENT_STATE"),
		GraphNotificationURL:     os.Getenv("GRAPH_NOTIFICATION_URL"),
		GraphPort:                os.Getenv("GRAPH_PORT"),
		GraphSubscriptionMinutes: os.Getenv("GRAPH_SUBSCRIPTION_MINUTES"),
		SharePointSearchScope:    os.Getenv("SHAREPOINT_SEARCH_SCOPE"),
		SharePointFolderPath:     os.Getenv("SHAREPOINT_FOLDER_PATH"),
		SharePointStartDate:      os.Getenv("SHAREPOINT_START_DATE"),
		OpenAIAPIKey:             os.Getenv("OPENAI_API_KEY"),
		ExtractPageImages:        envTruthy("GOODMEM_EXTRACT_PAGE_IMAGES"),
	}, nil
}

// sourceFromEnv reads SOURCE, defaulting to "sharepoint". Case-insensitive.
func sourceFromEnv() string {
	s := strings.ToLower(strings.TrimSpace(os.Getenv("SOURCE")))
	if s == "" {
		return "sharepoint"
	}
	return s
}

// HasServiceAccount reports whether a Google service-account key is configured
// (inline or by file). When false, the gdrive source falls back to Application
// Default Credentials (local `gcloud auth application-default login` / GCP host).
func (c *Config) HasServiceAccount() bool {
	return strings.TrimSpace(c.GDriveServiceAccount) != "" || strings.TrimSpace(c.GDriveServiceAccountFile) != ""
}

// ServiceAccountJSON returns the Google service-account key bytes, from the
// inline GDRIVE_SA_JSON if set, else the file at GDRIVE_SA_JSON_FILE.
func (c *Config) ServiceAccountJSON() ([]byte, error) {
	if strings.TrimSpace(c.GDriveServiceAccount) != "" {
		return []byte(c.GDriveServiceAccount), nil
	}
	if p := strings.TrimSpace(c.GDriveServiceAccountFile); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read GDRIVE_SA_JSON_FILE %q: %w", p, err)
		}
		return b, nil
	}
	return nil, fmt.Errorf("set GDRIVE_SA_JSON or GDRIVE_SA_JSON_FILE for the gdrive source")
}

// firstEnv returns the first non-empty value among the given env var names,
// mirroring the Python `os.getenv(A) or os.getenv(B) or ...` alias chains.
func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// envTruthy reports whether an env var is set to a truthy value, matching the
// Python check `value.strip().lower() in ("1","true","yes","on")`.
func envTruthy(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// ValidateSync checks the fields required to run a source→Goodmem sync (manual
// sync or the listener): Goodmem always, plus the selected source's credentials.
func (c *Config) ValidateSync() error {
	required := map[string]string{
		"GOODMEM_BASE_URL": c.GoodmemBaseURL,
		"GOODMEM_API_KEY":  c.GoodmemAPIKey,
	}
	switch c.Source {
	case "gdrive":
		required["GDRIVE_DRIVE_ID"] = c.GDriveDriveID
		// Auth is a service-account key (GDRIVE_SA_JSON / _FILE) or, if neither is
		// set, Application Default Credentials — so the key is not required here.
	case "sharepoint":
		required["AZURE_AD_CLIENT_ID"] = c.AzureClientID
		required["AZURE_AD_TENANT_ID"] = c.AzureTenantID
		required["AZURE_AD_CLIENT_SECRET"] = c.AzureClientSecret
		required["SHAREPOINT_SITE_URL"] = c.SharePointSiteURL
	default:
		return fmt.Errorf("unknown SOURCE %q (want \"sharepoint\" or \"gdrive\")", c.Source)
	}

	var missing []string
	for k, v := range required {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, k)
		}
	}
	// Note: GOODMEM_SPACE_ID / GOODMEM_EMBEDDER_ID are optional — the space is
	// resolved (or created from an embedder) at runtime; see ResolveSpaceID.
	if len(missing) > 0 {
		return fmt.Errorf("missing required config for source %q: %s", c.Source, strings.Join(missing, ", "))
	}
	return nil
}

// loadDotEnv loads KEY=VALUE lines from path for any key not already set in the
// environment. Blank lines and #-comments are ignored; surrounding quotes and
// inline comments are stripped (matching the .env.example conventions).
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // absent local .env is fine; prod uses real env
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, stripValue(val))
		}
	}
	return sc.Err()
}

// stripValue trims whitespace, an inline "# comment" on unquoted values, and
// surrounding single/double quotes.
func stripValue(v string) string {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, `"`) && !strings.HasPrefix(v, `'`) {
		if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
	}
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
	}
	return v
}
