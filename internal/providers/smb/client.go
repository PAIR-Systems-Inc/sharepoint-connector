// Package smb is the Windows-network-drive provider: it reads an SMB2/3 share —
// Windows Server, Samba, or a NAS appliance — via github.com/cloudsoda/go-smb2
// and adapts it to the connector's core/source.Source.
//
// Two properties of SMB shape everything here, and neither is a limitation of
// this code:
//
//   - There is no change feed. SharePoint has /delta and Google Drive has the
//     Changes API; SMB has neither, so "incremental" means walking the tree and
//     comparing modification times against a watermark (see adapter.go).
//   - There is no stable file id. Drive and Graph mint one per file; SMB
//     identifies a file only by its path, so the path *is* the identity.
//
// The share is reached through the standard io/fs interface, which keeps the
// SMB library confined to this file: everything above it works against an fs.FS
// and is tested with fstest.MapFS, no server required.
package smb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/cloudsoda/go-smb2"
)

// defaultPort is the SMB "direct TCP" port. Port 139 (NetBIOS session service)
// is legacy and not supported.
const defaultPort = "445"

// dialTimeout bounds connect + session setup, so an unreachable host fails the
// sync cycle instead of hanging it.
const dialTimeout = 30 * time.Second

// File is the slice of SMB file metadata the connector reads.
type File struct {
	// Path is the file's slash-separated path relative to the sync root. It is
	// the file's identity — see Adapter.MemNamespace.
	Path    string
	Name    string
	Size    int64
	ModTime time.Time
	// MimeType is inferred from the extension; SMB carries no content type.
	MimeType string
}

// Config describes which share to read and how to authenticate.
type Config struct {
	Host     string // host or host:port ("fileserver.corp.example.com")
	Share    string // share name ("Shared", from \\host\Shared)
	User     string
	Password string
	Domain   string // AD domain / workgroup; empty is fine for standalone servers
	Root     string // optional subdirectory within the share ("" = whole share)

	// Auth selects the mechanism: "ntlm" (default) or "kerberos". NTLM needs no
	// infrastructure; Kerberos needs a KDC and is required where NTLM is
	// disabled. The fields below apply only to Kerberos.
	Auth         string
	Realm        string // Kerberos realm, normally the AD domain UPPER-CASED
	KeytabPath   string // preferred for unattended runs — the connector renews its own tickets
	CCachePath   string // an existing credential cache; defaults to $KRB5CCNAME
	Krb5ConfPath string // defaults to /etc/krb5.conf
	SPN          string // override the derived cifs/<host> service principal
}

// usesKerberos reports whether this config authenticates with Kerberos.
func (c Config) usesKerberos() bool {
	return strings.EqualFold(strings.TrimSpace(c.Auth), AuthKerberos)
}

func (c Config) address() string {
	if _, _, err := net.SplitHostPort(c.Host); err == nil {
		return c.Host
	}
	return net.JoinHostPort(c.Host, defaultPort)
}

// Validate reports whether the config can be used to connect.
func (c Config) Validate() error {
	switch {
	case strings.TrimSpace(c.Host) == "":
		return errors.New("SMB_HOST is required")
	case strings.TrimSpace(c.Share) == "":
		return errors.New("SMB_SHARE is required")
	}
	if c.usesKerberos() {
		return c.validateKerberos()
	}
	if a := strings.TrimSpace(c.Auth); a != "" && !strings.EqualFold(a, AuthNTLM) {
		return fmt.Errorf("unknown SMB_AUTH %q (want %q or %q)", a, AuthNTLM, AuthKerberos)
	}
	if strings.TrimSpace(c.User) == "" {
		return errors.New("SMB_USER is required")
	}
	return nil
}

// mounter yields the share's filesystem. The production implementation dials
// SMB; tests substitute an in-memory fs.FS, which is why nothing above this
// interface imports the SMB library.
type mounter interface {
	// mount returns the share's filesystem, dialing if necessary.
	mount(ctx context.Context) (fs.FS, error)
	// invalidate drops the current connection so the next mount redials. Called
	// when an operation fails in a way that suggests the session is gone.
	invalidate()
	io.Closer
}

// Client reads files from one SMB share.
type Client struct {
	cfg Config
	m   mounter
}

// New builds a client for the configured share. It does not connect; the first
// operation dials, and the connection is then reused and re-established as
// needed.
func New(cfg Config) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Client{cfg: cfg, m: &smbMounter{cfg: cfg}}, nil
}

// newWithFS builds a client over a caller-supplied filesystem (tests).
func newWithFS(cfg Config, fsys fs.FS) *Client {
	return &Client{cfg: cfg, m: staticMounter{fsys: fsys}}
}

// Close releases the SMB session.
func (c *Client) Close() error { return c.m.Close() }

// Root is the configured subdirectory within the share ("" = the whole share).
func (c *Client) Root() string { return c.cfg.Root }

// Share is the configured share name.
func (c *Client) Share() string { return c.cfg.Share }

// Host is the configured server.
func (c *Client) Host() string { return c.cfg.Host }

// Walk visits every readable, non-skipped file under the sync root, calling fn
// for each. Walking is the only way to enumerate an SMB share, and it is also
// how incremental sync works (see Adapter.Delta), so both paths share it.
//
// Unreadable subtrees are skipped rather than failing the whole walk: a share
// commonly contains directories the connector's account cannot enter, and one
// of them must not block syncing everything else.
func (c *Client) Walk(ctx context.Context, fn func(File) error) error {
	fsys, err := c.m.mount(ctx)
	if err != nil {
		return err
	}
	root := walkRoot(c.cfg.Root)
	err = fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			// A directory we cannot open (access denied, transient) — skip that
			// subtree, keep the rest. Skipping the root itself is fatal, since
			// that means the whole sync would silently list nothing.
			if p == root {
				return err
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel := relativeTo(root, p)
		if d.IsDir() {
			if rel != "" && skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || skipFile(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // vanished mid-walk; the next cycle will see it
		}
		return fn(File{
			Path:     rel,
			Name:     d.Name(),
			Size:     info.Size(),
			ModTime:  info.ModTime().UTC(),
			MimeType: MimeTypeForName(d.Name()),
		})
	})
	if err != nil && isConnectionError(err) {
		c.m.invalidate()
	}
	return err
}

// Stat returns one file's current metadata, or fs.ErrNotExist if it is gone.
// A path that resolves to a directory returns (nil, nil), mirroring the other
// providers' "not a file" signal.
func (c *Client) Stat(ctx context.Context, relPath string) (*File, error) {
	fsys, err := c.m.mount(ctx)
	if err != nil {
		return nil, err
	}
	full := joinRoot(c.cfg.Root, relPath)
	info, err := fs.Stat(fsys, full)
	if err != nil {
		if isConnectionError(err) {
			c.m.invalidate()
		}
		return nil, err
	}
	if info.IsDir() {
		return nil, nil
	}
	name := path.Base(relPath)
	return &File{
		Path:     relPath,
		Name:     name,
		Size:     info.Size(),
		ModTime:  info.ModTime().UTC(),
		MimeType: MimeTypeForName(name),
	}, nil
}

// Open returns the file's bytes.
func (c *Client) Open(ctx context.Context, relPath string) (io.ReadCloser, error) {
	fsys, err := c.m.mount(ctx)
	if err != nil {
		return nil, err
	}
	f, err := fsys.Open(joinRoot(c.cfg.Root, relPath))
	if err != nil {
		if isConnectionError(err) {
			c.m.invalidate()
		}
		return nil, err
	}
	return f, nil
}

// --- path helpers -----------------------------------------------------------
//
// io/fs paths are always slash-separated and never start with "/" or contain
// ".." — the SMB library converts to backslashes on the wire.

func walkRoot(root string) string {
	root = strings.Trim(strings.ReplaceAll(root, "\\", "/"), "/")
	if root == "" {
		return "."
	}
	return path.Clean(root)
}

func joinRoot(root, rel string) string {
	r := walkRoot(root)
	rel = strings.Trim(strings.ReplaceAll(rel, "\\", "/"), "/")
	if r == "." {
		if rel == "" {
			return "."
		}
		return rel
	}
	if rel == "" {
		return r
	}
	return path.Join(r, rel)
}

// relativeTo converts a walk path into a path relative to the sync root, which
// is what the connector stores as the file's identity.
func relativeTo(root, p string) string {
	if root == "." {
		if p == "." {
			return ""
		}
		return p
	}
	if p == root {
		return ""
	}
	return strings.TrimPrefix(p, root+"/")
}

// --- skip policy ------------------------------------------------------------

// skipFile reports whether a filename is machine noise rather than content.
// Real shares are full of these, and each one would otherwise become a memory.
func skipFile(name string) bool {
	l := strings.ToLower(name)
	switch {
	case strings.HasPrefix(name, "~$"): // Office lock files for open documents
		return true
	case strings.HasPrefix(l, ".~lock."): // LibreOffice lock files
		return true
	case l == "thumbs.db", l == "desktop.ini", l == ".ds_store":
		return true
	case strings.HasPrefix(name, "."): // dotfiles
		return true
	}
	return false
}

// skipDir reports whether a directory should not be descended into.
func skipDir(name string) bool {
	l := strings.ToLower(name)
	switch l {
	case "$recycle.bin", "recycler", "system volume information", ".snapshot":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// --- content types ----------------------------------------------------------

// extMimeTypes maps the extensions Goodmem can extract text from to their
// content types. This table is explicit rather than relying solely on
// mime.TypeByExtension because Go's built-in table is small and the OS
// /etc/mime.types file is absent from the scratch container the connector ships
// in — so extension lookup there would return "" for .docx, .txt and friends,
// and every file would be filtered out as an unsupported type.
var extMimeTypes = map[string]string{
	".pdf":  "application/pdf",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".rtf":  "application/rtf",
	".txt":  "text/plain",
	".md":   "text/markdown",
	".csv":  "text/csv",
	".tsv":  "text/tab-separated-values",
	".log":  "text/plain",
	".htm":  "text/html",
	".html": "text/html",
	".json": "application/json",
	".xml":  "application/xml",
	".yaml": "text/yaml",
	".yml":  "text/yaml",
}

// MimeTypeForName infers a content type from a filename. Unknown extensions get
// application/octet-stream, which the engine's MIME filter then skips.
func MimeTypeForName(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if mt, ok := extMimeTypes[ext]; ok {
		return mt
	}
	if mt := mime.TypeByExtension(ext); mt != "" {
		// Strip parameters ("text/plain; charset=utf-8") so the stored content
		// type stays comparable to the table above.
		if i := strings.IndexByte(mt, ';'); i >= 0 {
			mt = strings.TrimSpace(mt[:i])
		}
		return mt
	}
	return "application/octet-stream"
}

// --- connection -------------------------------------------------------------

// isConnectionError reports whether err suggests the SMB session is no longer
// usable, so the next operation should redial. A missing file is emphatically
// not one of these.
func isConnectionError(err error) bool {
	if err == nil || errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return false
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne)
}

// staticMounter serves a fixed filesystem (tests).
type staticMounter struct{ fsys fs.FS }

func (s staticMounter) mount(context.Context) (fs.FS, error) { return s.fsys, nil }
func (s staticMounter) invalidate()                          {}
func (s staticMounter) Close() error                         { return nil }

// newInitiator builds the SPNEGO credential for the configured mechanism.
// Kerberos is resolved fresh on every (re)connect rather than cached, so a
// reconnect after a long outage acquires a current ticket instead of replaying
// an expired one.
func newInitiator(cfg Config) (smb2.Initiator, error) {
	if cfg.usesKerberos() {
		cfg.CCachePath = resolveCCachePath(cfg.CCachePath)
		return krb5Initiator(cfg)
	}
	return &smb2.NTLMInitiator{
		User:     cfg.User,
		Password: cfg.Password,
		Domain:   cfg.Domain,
	}, nil
}

// smbMounter holds the live SMB session and re-establishes it on demand.
type smbMounter struct {
	cfg Config

	mu      sync.Mutex
	session *smb2.Session
	share   *smb2.Share
	fsys    fs.FS
}

func (m *smbMounter) mount(ctx context.Context) (fs.FS, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fsys != nil {
		return m.fsys, nil
	}

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	init, err := newInitiator(m.cfg)
	if err != nil {
		return nil, err
	}
	d := &smb2.Dialer{Initiator: init}
	session, err := d.Dial(dialCtx, m.cfg.address())
	if err != nil {
		return nil, fmt.Errorf("smb connect %s: %w", m.cfg.address(), err)
	}
	share, err := session.Mount(m.cfg.Share)
	if err != nil {
		_ = session.Logoff()
		return nil, fmt.Errorf("smb mount %q: %w", m.cfg.Share, err)
	}
	m.session, m.share, m.fsys = session, share, share.DirFS(".")
	return m.fsys, nil
}

func (m *smbMounter) invalidate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeLocked()
}

func (m *smbMounter) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closeLocked()
}

func (m *smbMounter) closeLocked() error {
	var err error
	if m.share != nil {
		err = m.share.Umount()
		m.share = nil
	}
	if m.session != nil {
		if e := m.session.Logoff(); err == nil {
			err = e
		}
		m.session = nil
	}
	m.fsys = nil
	return err
}
