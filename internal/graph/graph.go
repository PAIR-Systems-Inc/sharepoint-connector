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

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time

	// OnTokenRefresh, if set, is called (holding the internal lock) with a
	// reason after a (re)authentication: "initial", "expired", or "401". It must
	// not call back into the client.
	OnTokenRefresh func(reason string)
}

// NewClient returns a Graph client for the given Azure app and SharePoint site.
func NewClient(clientID, tenantID, clientSecret, siteURL string) *Client {
	return &Client{
		clientID:     clientID,
		tenantID:     tenantID,
		clientSecret: clientSecret,
		siteURL:      siteURL,
		graphBase:    defaultGraphBase,
		loginBase:    defaultLoginBase,
		http:         &http.Client{Timeout: 60 * time.Second},
	}
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
	resp, err := c.http.PostForm(tokenURL, form)
	if err != nil {
		return fmt.Errorf("authentication request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{StatusCode: resp.StatusCode, Body: string(body)}
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
	var r io.Reader
	if reqBody != nil {
		r = bytes.NewReader(reqBody)
	}
	req, err := http.NewRequest(method, rawURL, r)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b, resp.StatusCode, nil
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

// Download fetches the bytes at a pre-authenticated @microsoft.graph.downloadUrl.
func (c *Client) Download(downloadURL string) ([]byte, error) {
	resp, err := c.http.Get(downloadURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: string(body)}
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
