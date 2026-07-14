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
	// Azure AD / SharePoint — always required.
	AzureClientID     string
	AzureTenantID     string
	AzureClientSecret string
	SharePointSiteURL string

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
		AzureClientID:            os.Getenv("AZURE_AD_CLIENT_ID"),
		AzureTenantID:            os.Getenv("AZURE_AD_TENANT_ID"),
		AzureClientSecret:        os.Getenv("AZURE_AD_CLIENT_SECRET"),
		SharePointSiteURL:        os.Getenv("SHAREPOINT_SITE_URL"),
		GoodmemBaseURL:           os.Getenv("GOODMEM_BASE_URL"),
		GoodmemAPIKey:            os.Getenv("GOODMEM_API_KEY"),
		GoodmemSpaceID:           os.Getenv("GOODMEM_SPACE_ID"),
		GoodmemEmbedderID:        os.Getenv("GOODMEM_EMBEDDER_ID"),
		GraphClientState:         os.Getenv("GRAPH_CLIENT_STATE"),
		GraphNotificationURL:     os.Getenv("GRAPH_NOTIFICATION_URL"),
		GraphPort:                os.Getenv("GRAPH_PORT"),
		GraphSubscriptionMinutes: os.Getenv("GRAPH_SUBSCRIPTION_MINUTES"),
		SharePointSearchScope:    os.Getenv("SHAREPOINT_SEARCH_SCOPE"),
		SharePointFolderPath:     os.Getenv("SHAREPOINT_FOLDER_PATH"),
		SharePointStartDate:      os.Getenv("SHAREPOINT_START_DATE"),
	}, nil
}

// ValidateSync checks the fields required to run a SharePoint→Goodmem sync
// (manual sync or the listener): Azure + SharePoint + Goodmem.
func (c *Config) ValidateSync() error {
	var missing []string
	for k, v := range map[string]string{
		"AZURE_AD_CLIENT_ID":     c.AzureClientID,
		"AZURE_AD_TENANT_ID":     c.AzureTenantID,
		"AZURE_AD_CLIENT_SECRET": c.AzureClientSecret,
		"SHAREPOINT_SITE_URL":    c.SharePointSiteURL,
		"GOODMEM_BASE_URL":       c.GoodmemBaseURL,
		"GOODMEM_API_KEY":        c.GoodmemAPIKey,
	} {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, k)
		}
	}
	if c.GoodmemSpaceID == "" && c.GoodmemEmbedderID == "" {
		missing = append(missing, "GOODMEM_SPACE_ID or GOODMEM_EMBEDDER_ID")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required config: %s", strings.Join(missing, ", "))
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
