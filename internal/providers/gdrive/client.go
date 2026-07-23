// Package gdrive is the Google Drive provider: it reads a Shared Drive via the
// official Google Drive v3 SDK (google.golang.org/api/drive/v3) with
// service-account OAuth2, and adapts it to the connector's core/source.Source.
// Read-only scope. The SDK is fully contained here — the engine never sees it.
package gdrive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

const (
	folderMime = "application/vnd.google-apps.folder"
	fileFields = "id,name,mimeType,modifiedTime,size,md5Checksum,trashed"
)

// DriveFile is the slice of Drive file metadata the connector reads.
type DriveFile struct {
	ID           string
	Name         string
	MimeType     string
	ModifiedTime string // RFC-3339
	Size         int64
	MD5          string
	Trashed      bool
}

// DriveChange is one entry from the Changes feed.
type DriveChange struct {
	FileID  string
	Removed bool
	File    *DriveFile // nil when removed
}

// Channel is a push-notification channel returned by changes.watch.
type Channel struct {
	ID         string
	ResourceID string
	Expiration string
}

// Client is a Google Drive v3 client scoped to one Shared Drive.
type Client struct {
	svc     *drive.Service
	driveID string
}

// New builds a Drive client from caller-supplied SDK options (prod: service-
// account credentials; tests: a fake endpoint + http client). driveID is the
// Shared Drive id.
func New(ctx context.Context, driveID string, opts ...option.ClientOption) (*Client, error) {
	svc, err := drive.NewService(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{svc: svc, driveID: driveID}, nil
}

// NewWithServiceAccount is the production constructor: read-only Drive access via
// a service-account JSON key. The service account must be a member of the Shared
// Drive (share the drive with its client_email).
func NewWithServiceAccount(ctx context.Context, serviceAccountJSON []byte, driveID string) (*Client, error) {
	if len(serviceAccountJSON) == 0 {
		return nil, errors.New("empty service account json")
	}
	return New(ctx, driveID,
		option.WithCredentialsJSON(serviceAccountJSON),
		option.WithScopes(drive.DriveReadonlyScope),
	)
}

func toFile(f *drive.File) *DriveFile {
	return &DriveFile{
		ID: f.Id, Name: f.Name, MimeType: f.MimeType,
		ModifiedTime: f.ModifiedTime, Size: f.Size, MD5: f.Md5Checksum, Trashed: f.Trashed,
	}
}

// ListFiles returns every non-folder, non-trashed file in the Shared Drive.
func (c *Client) ListFiles(ctx context.Context) ([]DriveFile, error) {
	var out []DriveFile
	err := c.svc.Files.List().
		Corpora("drive").DriveId(c.driveID).
		IncludeItemsFromAllDrives(true).SupportsAllDrives(true).
		Q("trashed=false").PageSize(1000).
		Fields("nextPageToken", googleapi.Field("files("+fileFields+")")).
		Pages(ctx, func(page *drive.FileList) error {
			for _, f := range page.Files {
				if f.MimeType == folderMime {
					continue
				}
				out = append(out, *toFile(f))
			}
			return nil
		})
	return out, err
}

// GetFile returns a file's current metadata. A 404 is returned as a *googleapi.Error
// (see IsNotFound); a folder returns (nil, nil).
func (c *Client) GetFile(ctx context.Context, id string) (*DriveFile, error) {
	f, err := c.svc.Files.Get(id).SupportsAllDrives(true).Fields(googleapi.Field(fileFields)).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	if f.MimeType == folderMime {
		return nil, nil
	}
	return toFile(f), nil
}

// Open returns the file's bytes: files.export for Google-native docs (Docs/Sheets/
// Slides), else files.get?alt=media for binary files. googleMime is the file's
// original Drive mimeType.
func (c *Client) Open(ctx context.Context, id, googleMime string) (io.ReadCloser, error) {
	if IsNativeDoc(googleMime) {
		target := ExportTarget(googleMime)
		if target == "" {
			return nil, fmt.Errorf("no export format for Google type %q", googleMime)
		}
		resp, err := c.svc.Files.Export(id, target).Context(ctx).Download()
		if err != nil {
			return nil, err
		}
		return resp.Body, nil
	}
	resp, err := c.svc.Files.Get(id).SupportsAllDrives(true).Context(ctx).Download()
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// StartPageToken returns a cursor positioned at "now" for the Changes feed.
func (c *Client) StartPageToken(ctx context.Context) (string, error) {
	r, err := c.svc.Changes.GetStartPageToken().SupportsAllDrives(true).DriveId(c.driveID).Context(ctx).Do()
	if err != nil {
		return "", err
	}
	return r.StartPageToken, nil
}

// Changes lists changes since pageToken and returns the next start token. A 410
// (invalid token) is returned as a *googleapi.Error (see IsCursorExpired).
func (c *Client) Changes(ctx context.Context, pageToken string) ([]DriveChange, string, error) {
	var out []DriveChange
	next := ""
	tok := pageToken
	for {
		resp, err := c.svc.Changes.List(tok).
			DriveId(c.driveID).IncludeItemsFromAllDrives(true).SupportsAllDrives(true).
			IncludeRemoved(true).PageSize(1000).
			Fields("nextPageToken", "newStartPageToken", googleapi.Field("changes(fileId,removed,file("+fileFields+"))")).
			Context(ctx).Do()
		if err != nil {
			return nil, "", err
		}
		for _, ch := range resp.Changes {
			dc := DriveChange{FileID: ch.FileId, Removed: ch.Removed}
			if ch.File != nil {
				dc.File = toFile(ch.File)
			}
			out = append(out, dc)
		}
		if resp.NewStartPageToken != "" {
			next = resp.NewStartPageToken
			break
		}
		if resp.NextPageToken == "" {
			break
		}
		tok = resp.NextPageToken
	}
	return out, next, nil
}

// Watch registers a push channel on the Changes feed. token is echoed back in the
// X-Goog-Channel-Token header for validation; ttl caps at 7 days (Drive's max).
func (c *Client) Watch(ctx context.Context, channelID, notifyURL, token string, ttl time.Duration) (Channel, error) {
	start, err := c.StartPageToken(ctx)
	if err != nil {
		return Channel{}, err
	}
	if ttl <= 0 || ttl > 7*24*time.Hour {
		ttl = 7 * 24 * time.Hour
	}
	ch := &drive.Channel{
		Id: channelID, Type: "web_hook", Address: notifyURL, Token: token,
		Expiration: time.Now().Add(ttl).UnixMilli(),
	}
	res, err := c.svc.Changes.Watch(start, ch).DriveId(c.driveID).SupportsAllDrives(true).Context(ctx).Do()
	if err != nil {
		return Channel{}, err
	}
	return Channel{ID: res.Id, ResourceID: res.ResourceId, Expiration: strconv.FormatInt(res.Expiration, 10)}, nil
}

// StopChannel stops a previously-created push channel.
func (c *Client) StopChannel(ctx context.Context, channelID, resourceID string) error {
	return c.svc.Channels.Stop(&drive.Channel{Id: channelID, ResourceId: resourceID}).Context(ctx).Do()
}

// IsNotFound reports whether err is a Drive 404.
func IsNotFound(err error) bool {
	var ge *googleapi.Error
	return errors.As(err, &ge) && ge.Code == 404
}

// IsCursorExpired reports whether err is a Drive 410 (expired change token).
func IsCursorExpired(err error) bool {
	var ge *googleapi.Error
	return errors.As(err, &ge) && ge.Code == 410
}

// --- Google-native export policy ---

// IsNativeDoc reports whether mime is a Google-native editor type (Docs/Sheets/
// Slides/etc.) that must be exported rather than downloaded.
func IsNativeDoc(mime string) bool {
	return strings.HasPrefix(mime, "application/vnd.google-apps.") && mime != folderMime
}

// ExportTarget maps a Google-native mime to the office format we export to, or ""
// if there is no supported export (the file is then skipped by the engine).
func ExportTarget(mime string) string {
	switch mime {
	case "application/vnd.google-apps.document":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document" // .docx
	case "application/vnd.google-apps.spreadsheet":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" // .xlsx
	case "application/vnd.google-apps.presentation":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation" // .pptx
	default:
		return ""
	}
}
