package smb

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/cloudsoda/go-smb2"
	krb5client "github.com/jcmturner/gokrb5/v8/client"
	krb5config "github.com/jcmturner/gokrb5/v8/config"
	"github.com/jcmturner/gokrb5/v8/credentials"
	"github.com/jcmturner/gokrb5/v8/keytab"
)

// Authentication methods. SMB negotiates the mechanism through SPNEGO, so both
// ride the same session setup — only the credential differs.
//
// NTLM needs nothing but a username and password and works against standalone
// servers, workgroups and NAS boxes. Kerberos needs a KDC (the Domain Controller
// in Active Directory) and is required wherever NTLM has been disabled, which
// Microsoft is progressively making the default.
const (
	AuthNTLM     = "ntlm"
	AuthKerberos = "kerberos"
)

// defaultKrb5Conf is where Kerberos configuration normally lives on Linux.
const defaultKrb5Conf = "/etc/krb5.conf"

// validateKerberos checks the fields Kerberos needs, in the order that produces
// the most useful complaint.
func (c Config) validateKerberos() error {
	if strings.TrimSpace(c.Realm) == "" {
		return errors.New("SMB_REALM is required for Kerberos (usually the AD domain in upper case, e.g. CORP.EXAMPLE.COM)")
	}
	hasKeytab := strings.TrimSpace(c.KeytabPath) != ""
	hasCCache := strings.TrimSpace(c.CCachePath) != ""
	hasPassword := c.Password != ""
	if !hasKeytab && !hasCCache && !hasPassword {
		return errors.New("Kerberos needs one of SMB_KEYTAB (preferred for unattended runs), SMB_CCACHE, or SMB_PASSWORD")
	}
	// A ccache already identifies its principal; the other two do not.
	if !hasCCache && strings.TrimSpace(c.User) == "" {
		return errors.New("SMB_USER is required for Kerberos with a keytab or password")
	}
	return nil
}

// targetSPN is the service principal the connector requests a ticket for.
//
// Kerberos identifies the service by name, so the SPN must match what is
// registered in the directory — normally cifs/<fqdn>. This is why an IP address
// in SMB_HOST cannot work with Kerberos: no SPN is registered for it, and the
// KDC will refuse to issue a ticket. Override with SMB_SPN when the registered
// name differs from the host we dial (a DFS target, or a CNAME).
func (c Config) targetSPN() (string, error) {
	if spn := strings.TrimSpace(c.SPN); spn != "" {
		return spn, nil
	}
	host := c.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" {
		return "", errors.New("SMB_HOST is required to derive the Kerberos SPN")
	}
	if net.ParseIP(host) != nil {
		return "", fmt.Errorf("SMB_HOST %q is an IP address; Kerberos identifies the service by name, "+
			"so use the server's FQDN (or set SMB_SPN explicitly, e.g. cifs/fileserver.corp.example.com)", host)
	}
	return "cifs/" + host, nil
}

// krb5Initiator builds the Kerberos credential from whichever source is
// configured. Precedence is keytab → ccache → password, i.e. most durable
// first: a keytab lets a long-running connector renew its own tickets, whereas
// a ccache holds a ticket that will eventually expire with nothing to renew it.
func krb5Initiator(cfg Config) (smb2.Initiator, error) {
	spn, err := cfg.targetSPN()
	if err != nil {
		return nil, err
	}

	confPath := strings.TrimSpace(cfg.Krb5ConfPath)
	if confPath == "" {
		confPath = defaultKrb5Conf
	}
	conf, err := krb5config.Load(confPath)
	if err != nil {
		return nil, fmt.Errorf("read Kerberos config %q (set SMB_KRB5_CONF if it lives elsewhere): %w", confPath, err)
	}

	var cl *krb5client.Client
	switch {
	case strings.TrimSpace(cfg.KeytabPath) != "":
		kt, err := keytab.Load(cfg.KeytabPath)
		if err != nil {
			return nil, fmt.Errorf("read keytab %q: %w", cfg.KeytabPath, err)
		}
		cl = krb5client.NewWithKeytab(cfg.User, cfg.Realm, kt, conf, krb5client.DisablePAFXFAST(true))

	case strings.TrimSpace(cfg.CCachePath) != "":
		cc, err := credentials.LoadCCache(cfg.CCachePath)
		if err != nil {
			return nil, fmt.Errorf("read credential cache %q: %w", cfg.CCachePath, err)
		}
		cl, err = krb5client.NewFromCCache(cc, conf, krb5client.DisablePAFXFAST(true))
		if err != nil {
			return nil, fmt.Errorf("load credential cache %q: %w", cfg.CCachePath, err)
		}

	default:
		cl = krb5client.NewWithPassword(cfg.User, cfg.Realm, cfg.Password, conf, krb5client.DisablePAFXFAST(true))
	}

	if err := cl.Login(); err != nil {
		return nil, fmt.Errorf("Kerberos login as %s@%s failed: %w%s", cfg.User, cfg.Realm, err, krbHint(err))
	}
	return &smb2.Krb5Initiator{Client: cl, TargetSPN: spn}, nil
}

// krbHint appends guidance for the Kerberos failures that are common and whose
// native error text does not suggest a fix.
func krbHint(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "skew"):
		return "\n  → clock skew: Kerberos rejects tickets when the client and KDC clocks differ " +
			"(5 minutes by default). Sync this host's time (NTP/chrony) and retry."
	case strings.Contains(s, "kdc_err_c_principal_unknown"), strings.Contains(s, "principal unknown"):
		return "\n  → the KDC does not know that principal: check SMB_USER and SMB_REALM " +
			"(the realm is normally the AD domain in UPPER CASE)."
	case strings.Contains(s, "kdc_err_preauth_failed"), strings.Contains(s, "preauthentication"):
		return "\n  → pre-authentication failed: wrong password, or a keytab whose key no longer " +
			"matches the account (a password change invalidates an old keytab)."
	case strings.Contains(s, "no such file"), strings.Contains(s, "cannot find"):
		return "\n  → check the keytab/ccache path is readable by the user running the connector."
	case strings.Contains(s, "lookup"), strings.Contains(s, "no route"), strings.Contains(s, "server not found"):
		return "\n  → cannot reach the KDC: Kerberos needs DNS resolution of the realm and network " +
			"access to the Domain Controller, not just to the file server."
	}
	return ""
}

// resolveCCachePath falls back to the conventional KRB5CCNAME environment
// variable when no explicit cache is configured, matching what other Kerberos
// tooling on the host will already be using.
func resolveCCachePath(configured string) string {
	if strings.TrimSpace(configured) != "" {
		return configured
	}
	v := strings.TrimSpace(os.Getenv("KRB5CCNAME"))
	// KRB5CCNAME is typed: FILE:/path is a file cache, but DIR:, KEYRING: and
	// KCM: name caches this library cannot read from a path.
	if after, ok := strings.CutPrefix(v, "FILE:"); ok {
		return after
	}
	if strings.Contains(v, ":") {
		return "" // a cache type we cannot load; fall through to other credentials
	}
	return v
}
