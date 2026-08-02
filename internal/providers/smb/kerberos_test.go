package smb

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestValidate_AuthMethodSelection(t *testing.T) {
	base := Config{Host: "fs1.corp.example.com", Share: "data"}

	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // substring; "" means valid
	}{
		{"ntlm default needs user+nothing else", func(c *Config) { c.User = "u" }, ""},
		{"ntlm explicit", func(c *Config) { c.Auth = "ntlm"; c.User = "u" }, ""},
		{"ntlm case-insensitive", func(c *Config) { c.Auth = "NTLM"; c.User = "u" }, ""},
		{"unknown method is rejected", func(c *Config) { c.Auth = "ldap"; c.User = "u" }, "unknown SMB_AUTH"},
		{"ntlm without user", func(c *Config) {}, "SMB_USER"},

		{"kerberos needs a realm", func(c *Config) {
			c.Auth = "kerberos"
			c.User = "u"
			c.KeytabPath = "/etc/k.keytab"
		}, "SMB_REALM"},
		{"kerberos needs a credential", func(c *Config) {
			c.Auth = "kerberos"
			c.User = "u"
			c.Realm = "CORP.EXAMPLE.COM"
		}, "SMB_KEYTAB"},
		{"kerberos with keytab", func(c *Config) {
			c.Auth = "kerberos"
			c.User = "u"
			c.Realm = "CORP.EXAMPLE.COM"
			c.KeytabPath = "/etc/k.keytab"
		}, ""},
		{"kerberos with password", func(c *Config) {
			c.Auth = "kerberos"
			c.User = "u"
			c.Realm = "CORP.EXAMPLE.COM"
			c.Password = "pw"
		}, ""},
		{"kerberos with keytab still needs a user", func(c *Config) {
			c.Auth = "kerberos"
			c.Realm = "CORP.EXAMPLE.COM"
			c.KeytabPath = "/etc/k.keytab"
		}, "SMB_USER"},
		// A ccache already names its principal, so no user is required.
		{"kerberos with ccache needs no user", func(c *Config) {
			c.Auth = "kerberos"
			c.Realm = "CORP.EXAMPLE.COM"
			c.CCachePath = "/tmp/krb5cc_1000"
		}, ""},
		// $KRB5CCNAME is a legitimate credential source, so it must satisfy the
		// check too — validating the raw field alone rejected a configuration
		// that would in fact have worked.
		{"kerberos with KRB5CCNAME only", func(c *Config) {
			c.Auth = "kerberos"
			c.Realm = "CORP.EXAMPLE.COM"
			os.Setenv("KRB5CCNAME", "FILE:/tmp/krb5cc_env")
		}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Isolate each case from a developer's real Kerberos session and from
			// whatever a previous case set: an inherited KRB5CCNAME satisfies the
			// credential check and would mask the negative cases.
			t.Setenv("KRB5CCNAME", "")
			cfg := base
			tc.mutate(&cfg)
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate() = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestUsesKerberos(t *testing.T) {
	for in, want := range map[string]bool{
		"":          false,
		"ntlm":      false,
		"kerberos":  true,
		"Kerberos":  true,
		"KERBEROS":  true,
		" kerberos": true,
	} {
		if got := (Config{Auth: in}).usesKerberos(); got != want {
			t.Errorf("usesKerberos(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestTargetSPN(t *testing.T) {
	t.Run("derived from the hostname", func(t *testing.T) {
		got, err := Config{Host: "fs1.corp.example.com"}.targetSPN()
		if err != nil || got != "cifs/fs1.corp.example.com" {
			t.Errorf("targetSPN() = (%q, %v), want cifs/fs1.corp.example.com", got, err)
		}
	})

	t.Run("port is stripped", func(t *testing.T) {
		got, err := Config{Host: "fs1.corp.example.com:445"}.targetSPN()
		if err != nil || got != "cifs/fs1.corp.example.com" {
			t.Errorf("targetSPN() = (%q, %v), want the port stripped", got, err)
		}
	})

	t.Run("explicit override wins", func(t *testing.T) {
		got, err := Config{Host: "10.0.0.5", SPN: "cifs/real-name.corp.example.com"}.targetSPN()
		if err != nil || got != "cifs/real-name.corp.example.com" {
			t.Errorf("targetSPN() = (%q, %v), want the override", got, err)
		}
	})

	// Kerberos identifies the service by name. An IP has no registered SPN, so
	// the KDC refuses the ticket — better to say so up front than to let it fail
	// deep inside the exchange with an opaque error.
	t.Run("an IP address is refused with an explanation", func(t *testing.T) {
		for _, ip := range []string{"10.0.0.5", "10.0.0.5:445", "fe80::1"} {
			_, err := Config{Host: ip}.targetSPN()
			if err == nil {
				t.Errorf("targetSPN(%q) = nil error, want a refusal", ip)
				continue
			}
			if !strings.Contains(err.Error(), "FQDN") {
				t.Errorf("targetSPN(%q) error = %q, should point at using the FQDN", ip, err)
			}
		}
	})
}

func TestResolveCCachePath(t *testing.T) {
	t.Setenv("KRB5CCNAME", "")
	if got := resolveCCachePath("/explicit/path"); got != "/explicit/path" {
		t.Errorf("explicit path = %q, want it preserved", got)
	}

	t.Run("falls back to KRB5CCNAME", func(t *testing.T) {
		t.Setenv("KRB5CCNAME", "/tmp/krb5cc_1000")
		if got := resolveCCachePath(""); got != "/tmp/krb5cc_1000" {
			t.Errorf("got %q, want the env value", got)
		}
	})

	t.Run("FILE: prefix is stripped", func(t *testing.T) {
		t.Setenv("KRB5CCNAME", "FILE:/tmp/krb5cc_1000")
		if got := resolveCCachePath(""); got != "/tmp/krb5cc_1000" {
			t.Errorf("got %q, want the path without the FILE: prefix", got)
		}
	})

	// DIR:, KEYRING: and KCM: name caches this library cannot open as a file.
	// Returning them would produce a confusing "no such file" instead of falling
	// through to a keytab or password.
	t.Run("unsupported cache types are ignored", func(t *testing.T) {
		for _, v := range []string{"DIR:/run/user/1000/krb5cc", "KEYRING:persistent:1000", "KCM:1000"} {
			t.Setenv("KRB5CCNAME", v)
			if got := resolveCCachePath(""); got != "" {
				t.Errorf("resolveCCachePath with KRB5CCNAME=%q = %q, want \"\"", v, got)
			}
		}
	})
}

// The Kerberos failures worth a hint are the ones whose native message does not
// suggest a fix — clock skew above all, which is the classic silent killer.
func TestKrbHint(t *testing.T) {
	cases := []struct{ err, want string }{
		{"KRB_AP_ERR_SKEW Clock skew too great", "clock skew"},
		{"KDC_ERR_C_PRINCIPAL_UNKNOWN", "SMB_REALM"},
		{"KDC_ERR_PREAUTH_FAILED", "keytab"},
		{"open /etc/x.keytab: no such file or directory", "readable"},
		{"lookup dc.corp.example.com: no such host", "KDC"},
	}
	for _, c := range cases {
		got := krbHint(errors.New(c.err))
		if !strings.Contains(strings.ToLower(got), strings.ToLower(c.want)) {
			t.Errorf("krbHint(%q) = %q, want it to mention %q", c.err, got, c.want)
		}
	}
	if krbHint(errors.New("something entirely unrelated")) != "" {
		t.Error("an unrecognized error should get no hint rather than a misleading one")
	}
}

// Kerberos config problems must surface before dialing, so an operator sees the
// real cause instead of a connection error.
func TestKrb5Initiator_FailsEarlyOnBadConfig(t *testing.T) {
	_, err := krb5Initiator(Config{
		Host: "fs1.corp.example.com", Share: "data", User: "svc",
		Realm: "CORP.EXAMPLE.COM", KeytabPath: "/nonexistent/x.keytab",
		Krb5ConfPath: "/nonexistent/krb5.conf",
	})
	if err == nil {
		t.Fatal("want an error for a missing krb5.conf")
	}
	if !strings.Contains(err.Error(), "SMB_KRB5_CONF") {
		t.Errorf("err = %q, should name the setting that fixes it", err)
	}
}
