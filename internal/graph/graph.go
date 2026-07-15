// Package graph is a lean, hand-rolled Microsoft Graph client for reading a
// SharePoint site's drive. It is a port of sharepoint_client.py: OAuth2
// client-credentials auth plus the small slice of Graph we use (site, drives,
// drive children, get item by id, drive delta, download). No Graph SDK.
package graph

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultGraphBase = "https://graph.microsoft.com/v1.0"
	defaultLoginBase = "https://login.microsoftonline.com"

	// Max token validity (minutes) before proactive refresh, capped by Azure's
	// expires_in. Overridable via AZURE_AD_OAUTH_TOKEN_MINUTES.
	tokenMinutesDefault = 60
	tokenMinutesMin     = 10
	tokenMinutesMax     = 1440

	// Retry/backoff for Graph throttling (429) and transient 5xx/network errors.
	// Overridable via GRAPH_MAX_RETRIES (clamped to [0, maxRetriesCeil]).
	maxRetriesDefault = 5
	maxRetriesCeil    = 10
	baseBackoff       = 500 * time.Millisecond
	maxBackoff        = 30 * time.Second
	// Cap on honoring a server-provided Retry-After so a hostile/absurd value
	// cannot stall the connector indefinitely.
	maxRetryAfter = 120 * time.Second
)

// FileInfo is the flattened metadata for a SharePoint drive file. JSON tags
// match the Python file_info dicts so serialized output is identical.
type FileInfo struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	WebURL           string `json:"web_url"`
	DownloadURL      string `json:"download_url"`
	Size             int64  `json:"size"`
	CreatedDateTime  string `json:"created_datetime"`
	ModifiedDateTime string `json:"modified_datetime"`
	CreatedBy        string `json:"created_by"`
	ModifiedBy       string `json:"modified_by"`
	MimeType         string `json:"mime_type"`
	FileHash         string `json:"file_hash"`
	RelativePath     string `json:"relative_path"`
}

// Item is a drive/delta item: a file, a folder, or a deleted stub.
type Item struct {
	ID       string
	Name     string
	Deleted  bool
	IsFile   bool
	IsFolder bool
	File     FileInfo // valid when IsFile
}

// Drive is a document library.
type Drive struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// HTTPError is a non-2xx Graph response.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	b := e.Body
	if len(b) > 500 {
		b = b[:500]
	}
	return fmt.Sprintf("graph HTTP %d: %s", e.StatusCode, b)
}

// Client is a Microsoft Graph client using the client-credentials flow.
type Client struct {
	clientID     string
	tenantID     string
	clientSecret string
	siteURL      string

	graphBase string
	loginBase string
	http      *http.Client

	// Retry/backoff tuning. sleepFn is time.Sleep (overridable in tests).
	maxRetries  int
	baseBackoff time.Duration
	maxBackoff  time.Duration
	sleepFn     func(time.Duration)

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time

	// OnTokenRefresh, if set, is called (holding the internal lock) with a
	// reason after a (re)authentication: "initial", "expired", or "401". It must
	// not call back into the client.
	OnTokenRefresh func(reason string)

	// OnThrottle, if set, is called before each backoff sleep caused by a
	// throttle (429) or transient (5xx/network) response: the HTTP status (0 for
	// a network error), the upcoming retry number (1-based), and the Retry-After
	// the server asked for (0 if none). For observability/logging; must not call
	// back into the client.
	OnThrottle func(status, attempt int, retryAfter time.Duration)
}

// Option customizes a Client at construction.
type Option func(*Client)

// WithBaseURLs overrides the Graph API and OAuth login base URLs — for sovereign
// clouds or, in tests, an httptest server. Empty values are left at the default.
func WithBaseURLs(graphBase, loginBase string) Option {
	return func(c *Client) {
		if graphBase != "" {
			c.graphBase = graphBase
		}
		if loginBase != "" {
			c.loginBase = loginBase
		}
	}
}

// NewClient returns a Graph client for the given Azure app and SharePoint site.
func NewClient(clientID, tenantID, clientSecret, siteURL string, opts ...Option) *Client {
	c := &Client{
		clientID:     clientID,
		tenantID:     tenantID,
		clientSecret: clientSecret,
		siteURL:      siteURL,
		graphBase:    defaultGraphBase,
		loginBase:    defaultLoginBase,
		http:         &http.Client{Timeout: 60 * time.Second},
		maxRetries:   maxRetriesFromEnv(),
		baseBackoff:  baseBackoff,
		maxBackoff:   maxBackoff,
		sleepFn:      time.Sleep,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// maxRetriesFromEnv reads GRAPH_MAX_RETRIES, clamped to [0, maxRetriesCeil].
func maxRetriesFromEnv() int {
	s := strings.TrimSpace(os.Getenv("GRAPH_MAX_RETRIES"))
	if s == "" {
		return maxRetriesDefault
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		return maxRetriesDefault
	}
	if v > maxRetriesCeil {
		return maxRetriesCeil
	}
	return v
}

// ValidateTokenRefreshBuffer checks AZURE_AD_OAUTH_TOKEN_MINUTES is within
// [10, 1440] when set. Call once at startup.
func ValidateTokenRefreshBuffer() error {
	s := strings.TrimSpace(os.Getenv("AZURE_AD_OAUTH_TOKEN_MINUTES"))
	if s == "" {
		return nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("AZURE_AD_OAUTH_TOKEN_MINUTES must be an integer (got %q); allowed %d–%d minutes", s, tokenMinutesMin, tokenMinutesMax)
	}
	if v < tokenMinutesMin || v > tokenMinutesMax {
		return fmt.Errorf("AZURE_AD_OAUTH_TOKEN_MINUTES=%d out of range; allowed %d–%d minutes", v, tokenMinutesMin, tokenMinutesMax)
	}
	return nil
}

func maxTokenMinutes() int {
	s := strings.TrimSpace(os.Getenv("AZURE_AD_OAUTH_TOKEN_MINUTES"))
	if s == "" {
		return tokenMinutesDefault
	}
	if v, err := strconv.Atoi(s); err == nil {
		return v
	}
	return tokenMinutesDefault
}

// authenticate runs the client-credentials flow. Caller must hold c.mu.
func (c *Client) authenticate() error {
	firstAuth := c.accessToken == ""
	tokenURL := fmt.Sprintf("%s/%s/oauth2/v2.0/token", c.loginBase, c.tenantID)
	form := url.Values{
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
		"scope":         {"https://graph.microsoft.com/.default"},
		"grant_type":    {"client_credentials"},
	}
	body, status, err := c.httpDoRetry(func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return req, nil
	})
	if err != nil {
		return fmt.Errorf("authentication request failed: %w", err)
	}
	if status < 200 || status >= 300 {
		return &HTTPError{StatusCode: status, Body: string(body)}
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return err
	}
	if tr.AccessToken == "" {
		return errors.New("no access token received")
	}
	expiresIn := tr.ExpiresIn
	if expiresIn == 0 {
		expiresIn = 3599
	}
	maxValidSeconds := expiresIn
	if cap := max(1, maxTokenMinutes()) * 60; cap < maxValidSeconds {
		maxValidSeconds = cap
	}
	c.accessToken = tr.AccessToken
	c.expiresAt = time.Now().UTC().Add(time.Duration(maxValidSeconds) * time.Second)
	if c.OnTokenRefresh != nil && firstAuth {
		c.OnTokenRefresh("initial")
	}
	return nil
}

// token returns a valid bearer token, refreshing proactively if expired.
func (c *Client) token() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now().UTC()
	if c.accessToken == "" || (!c.expiresAt.IsZero() && !now.Before(c.expiresAt)) {
		hadToken := !c.expiresAt.IsZero()
		if err := c.authenticate(); err != nil {
			return "", err
		}
		if c.OnTokenRefresh != nil && hadToken {
			c.OnTokenRefresh("expired")
		}
	}
	return c.accessToken, nil
}

// do sends an authenticated request; on 401 it re-authenticates once and retries.
func (c *Client) do(method, rawURL string, reqBody []byte) (body []byte, status int, err error) {
	tok, err := c.token()
	if err != nil {
		return nil, 0, err
	}
	body, status, err = c.send(method, rawURL, tok, reqBody)
	if err != nil {
		return nil, 0, err
	}
	if status == http.StatusUnauthorized {
		c.mu.Lock()
		reErr := c.authenticate()
		if reErr == nil && c.OnTokenRefresh != nil {
			c.OnTokenRefresh("401")
		}
		tok = c.accessToken
		c.mu.Unlock()
		if reErr != nil {
			return body, status, nil
		}
		return c.send(method, rawURL, tok, reqBody)
	}
	return body, status, nil
}

func (c *Client) send(method, rawURL, token string, reqBody []byte) ([]byte, int, error) {
	return c.httpDoRetry(func() (*http.Request, error) {
		var r io.Reader
		if reqBody != nil {
			r = bytes.NewReader(reqBody)
		}
		req, err := http.NewRequest(method, rawURL, r)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	})
}

// httpDoRetry executes reqFn — which must build a fresh *http.Request on each
// call, since a request body reader is consumed per attempt — retrying on Graph
// throttling (429) and transient errors (5xx, network failures). It honors a
// Retry-After header when present, otherwise uses jittered exponential backoff,
// up to c.maxRetries additional attempts. It returns the final response's body
// and status (401 and other non-retryable statuses pass straight through so the
// caller's own handling — e.g. do()'s 401 re-auth — still applies).
func (c *Client) httpDoRetry(reqFn func() (*http.Request, error)) ([]byte, int, error) {
	for attempt := 0; ; attempt++ {
		req, err := reqFn()
		if err != nil {
			return nil, 0, err
		}
		// Whether repeating THIS request on an ambiguous failure is safe. A 429
		// means "not processed, back off" and is always safe to retry; a 5xx or a
		// network error is ambiguous (the server may have already applied a POST),
		// so those are retried only for idempotent methods — otherwise a retried
		// POST /subscriptions could create a duplicate.
		retrySafe := isRetrySafeMethod(req.Method)
		resp, err := c.http.Do(req)
		if err != nil {
			if retrySafe && attempt < c.maxRetries {
				c.notifyThrottle(0, attempt+1, 0)
				c.sleepFn(c.backoff(attempt, 0))
				continue
			}
			return nil, 0, err
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		retryable := shouldRetryStatus(resp.StatusCode) &&
			(retrySafe || resp.StatusCode == http.StatusTooManyRequests)
		if retryable && attempt < c.maxRetries {
			ra := parseRetryAfter(resp.Header.Get("Retry-After"))
			c.notifyThrottle(resp.StatusCode, attempt+1, ra)
			c.sleepFn(c.backoff(attempt, ra))
			continue
		}
		return b, resp.StatusCode, nil
	}
}

// isRetrySafeMethod reports whether repeating a request with this method is safe
// on an ambiguous failure (network error or 5xx). POST is not — a retried
// create could duplicate server-side state — so POST is retried only on 429.
func isRetrySafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions:
		return true
	}
	return false
}

func (c *Client) notifyThrottle(status, attempt int, retryAfter time.Duration) {
	if c.OnThrottle != nil {
		c.OnThrottle(status, attempt, retryAfter)
	}
}

// shouldRetryStatus reports whether an HTTP status is a Graph throttle (429) or
// a transient server error worth retrying. Whether a given method may actually
// be retried on a 5xx is gated separately by isRetrySafeMethod.
func shouldRetryStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// backoff returns how long to wait before the next attempt. A positive
// retryAfter (from a Retry-After header) is honored verbatim, capped at
// maxRetryAfter; otherwise it is full-jittered exponential backoff — a random
// duration in [d/2, d] where d = baseBackoff·2^attempt capped at maxBackoff.
func (c *Client) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > maxRetryAfter {
			return maxRetryAfter
		}
		return retryAfter
	}
	d := c.baseBackoff
	for i := 0; i < attempt && d < c.maxBackoff; i++ {
		d *= 2
	}
	if d > c.maxBackoff {
		d = c.maxBackoff
	}
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

// parseRetryAfter parses a Retry-After header, which is either delta-seconds or
// an HTTP-date. Returns 0 for empty/unparseable/past values.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// reqJSON sends method+url with an optional JSON body and unmarshals a 2xx JSON
// response into out (out may be nil).
func (c *Client) reqJSON(method, rawURL string, reqBody, out any) error {
	var b []byte
	if reqBody != nil {
		var err error
		if b, err = json.Marshal(reqBody); err != nil {
			return err
		}
	}
	respBody, status, err := c.do(method, rawURL, b)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return &HTTPError{StatusCode: status, Body: string(respBody)}
	}
	if out != nil {
		return json.Unmarshal(respBody, out)
	}
	return nil
}

// getJSON does an authenticated GET and unmarshals a 2xx JSON body into out.
func (c *Client) getJSON(rawURL string, out any) error {
	return c.reqJSON(http.MethodGet, rawURL, nil, out)
}

// GetSiteID resolves the Graph site id from the configured site URL.
func (c *Client) GetSiteID() (string, error) {
	hostname, sitePath := parseSiteURL(c.siteURL)
	var site struct {
		ID string `json:"id"`
	}
	if err := c.getJSON(fmt.Sprintf("%s/sites/%s:%s", c.graphBase, hostname, sitePath), &site); err != nil {
		return "", err
	}
	return site.ID, nil
}

// GetDrives lists the document libraries for a site (resolving the site if "").
func (c *Client) GetDrives(siteID string) ([]Drive, error) {
	if siteID == "" {
		id, err := c.GetSiteID()
		if err != nil {
			return nil, err
		}
		siteID = id
	}
	var data struct {
		Value []Drive `json:"value"`
	}
	if err := c.getJSON(fmt.Sprintf("%s/sites/%s/drives", c.graphBase, siteID), &data); err != nil {
		return nil, err
	}
	return data.Value, nil
}

// ListFiles lists files in a drive. If driveID is "", the first drive is used;
// if folderPath is "", the drive root is listed. recursive descends subfolders.
func (c *Client) ListFiles(driveID, folderPath string, recursive bool, siteID string) ([]FileInfo, error) {
	if siteID == "" {
		id, err := c.GetSiteID()
		if err != nil {
			return nil, err
		}
		siteID = id
	}
	if driveID == "" {
		drives, err := c.GetDrives(siteID)
		if err != nil {
			return nil, err
		}
		if len(drives) == 0 {
			return nil, errors.New("no drives found")
		}
		driveID = drives[0].ID
	}

	var endpoint string
	if folderPath != "" {
		endpoint = fmt.Sprintf("%s/drives/%s/root:/%s:/children", c.graphBase, driveID, encodeFolderPath(folderPath))
	} else {
		endpoint = fmt.Sprintf("%s/drives/%s/root/children", c.graphBase, driveID)
	}

	items, err := c.getChildren(endpoint)
	if err != nil {
		return nil, err
	}
	var files []FileInfo
	for _, it := range items {
		switch {
		case it.File != nil:
			fi := formatFileInfo(it)
			fi.RelativePath = it.Name
			files = append(files, fi)
		case it.Folder != nil && recursive:
			files = append(files, c.filesFromFolder(driveID, it.ID, it.Name)...)
		}
	}
	return files, nil
}

// filesFromFolder recursively lists files under a folder. On error it warns and
// returns what it has (mirroring the Python reference).
func (c *Client) filesFromFolder(driveID, folderID, parentPath string) []FileInfo {
	items, err := c.getChildren(fmt.Sprintf("%s/drives/%s/items/%s/children", c.graphBase, driveID, folderID))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to get files from folder %s: %v\n", folderID, err)
		return nil
	}
	var files []FileInfo
	for _, it := range items {
		rel := strings.TrimPrefix(parentPath+"/"+it.Name, "/")
		switch {
		case it.File != nil:
			fi := formatFileInfo(it)
			fi.RelativePath = rel
			files = append(files, fi)
		case it.Folder != nil:
			files = append(files, c.filesFromFolder(driveID, it.ID, rel)...)
		}
	}
	return files
}

// getChildren fetches all children of an item endpoint, following pagination.
func (c *Client) getChildren(endpoint string) ([]graphItem, error) {
	var all []graphItem
	u := endpoint
	for {
		var data struct {
			Value    []graphItem `json:"value"`
			NextLink string      `json:"@odata.nextLink"`
		}
		if err := c.getJSON(u, &data); err != nil {
			return nil, err
		}
		all = append(all, data.Value...)
		if data.NextLink == "" {
			break
		}
		u = data.NextLink
	}
	return all, nil
}

// GetFileByID returns the file at itemID, or (nil, nil) if it is a folder.
// A 404 surfaces as an *HTTPError so callers can distinguish "deleted" from a
// transient failure.
func (c *Client) GetFileByID(driveID, itemID string) (*FileInfo, error) {
	var it graphItem
	if err := c.getJSON(fmt.Sprintf("%s/drives/%s/items/%s", c.graphBase, driveID, itemID), &it); err != nil {
		return nil, err
	}
	if it.File == nil {
		return nil, nil
	}
	fi := formatFileInfo(it)
	return &fi, nil
}

// DriveDelta returns changes since deltaLink (or all, or just the latest token
// when tokenLatest). On 410 Gone (invalid token) it returns (nil, "", nil) so
// the caller runs a full sync and re-bootstraps.
func (c *Client) DriveDelta(driveID, deltaLink string, tokenLatest bool) (items []Item, newDeltaLink string, err error) {
	var u string
	switch {
	case tokenLatest:
		u = fmt.Sprintf("%s/drives/%s/root/delta?token=latest", c.graphBase, driveID)
	case deltaLink != "":
		u = deltaLink
	default:
		u = fmt.Sprintf("%s/drives/%s/root/delta", c.graphBase, driveID)
	}
	for {
		body, status, derr := c.do(http.MethodGet, u, nil)
		if derr != nil {
			return nil, "", derr
		}
		if status == http.StatusGone {
			return nil, "", nil
		}
		if status < 200 || status >= 300 {
			return nil, "", &HTTPError{StatusCode: status, Body: string(body)}
		}
		var data struct {
			Value     []graphItem `json:"value"`
			NextLink  string      `json:"@odata.nextLink"`
			DeltaLink string      `json:"@odata.deltaLink"`
		}
		if e := json.Unmarshal(body, &data); e != nil {
			return nil, "", e
		}
		for _, it := range data.Value {
			items = append(items, toItem(it))
		}
		if data.DeltaLink != "" {
			return items, data.DeltaLink, nil
		}
		if data.NextLink == "" {
			return items, "", nil
		}
		u = data.NextLink
	}
}

// Download fetches the bytes at a pre-authenticated @microsoft.graph.downloadUrl,
// with the same throttle/transient retry as authenticated calls.
func (c *Client) Download(downloadURL string) ([]byte, error) {
	body, status, err := c.httpDoRetry(func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, downloadURL, nil)
	})
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, &HTTPError{StatusCode: status, Body: string(body)}
	}
	return body, nil
}

// --- internal item parsing ---

type graphItem struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	WebURL      string           `json:"webUrl"`
	DownloadURL string           `json:"@microsoft.graph.downloadUrl"`
	Size        int64            `json:"size"`
	Created     string           `json:"createdDateTime"`
	Modified    string           `json:"lastModifiedDateTime"`
	CreatedBy   userWrap         `json:"createdBy"`
	ModifiedBy  userWrap         `json:"lastModifiedBy"`
	File        *fileFacet       `json:"file"`
	Folder      *json.RawMessage `json:"folder"`
	Deleted     *json.RawMessage `json:"deleted"`
}

type userWrap struct {
	User struct {
		DisplayName string `json:"displayName"`
	} `json:"user"`
}

type fileFacet struct {
	MimeType string `json:"mimeType"`
	Hashes   struct {
		Sha1Hash string `json:"sha1Hash"`
	} `json:"hashes"`
}

func formatFileInfo(it graphItem) FileInfo {
	fi := FileInfo{
		ID:               it.ID,
		Name:             it.Name,
		WebURL:           it.WebURL,
		DownloadURL:      it.DownloadURL,
		Size:             it.Size,
		CreatedDateTime:  it.Created,
		ModifiedDateTime: it.Modified,
		CreatedBy:        it.CreatedBy.User.DisplayName,
		ModifiedBy:       it.ModifiedBy.User.DisplayName,
	}
	if it.File != nil {
		fi.MimeType = it.File.MimeType
		fi.FileHash = it.File.Hashes.Sha1Hash
	}
	return fi
}

func toItem(it graphItem) Item {
	item := Item{
		ID:       it.ID,
		Name:     it.Name,
		Deleted:  it.Deleted != nil,
		IsFile:   it.File != nil,
		IsFolder: it.Folder != nil,
	}
	if it.File != nil {
		item.File = formatFileInfo(it)
	}
	return item
}

// parseSiteURL splits a SharePoint site URL into (hostname, server-relative
// path), matching the Python get_site_id logic.
func parseSiteURL(siteURL string) (hostname, sitePath string) {
	s := strings.TrimPrefix(strings.TrimPrefix(siteURL, "https://"), "http://")
	parts := strings.Split(s, "/")
	hostname = parts[0]
	if len(parts) > 1 {
		sitePath = "/" + strings.Join(parts[1:], "/")
	} else {
		sitePath = "/"
	}
	return hostname, sitePath
}

func encodeFolderPath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
