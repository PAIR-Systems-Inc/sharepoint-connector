package server

import (
	"path/filepath"
	"testing"
)

func TestDeltaStore(t *testing.T) {
	d := deltaStore{path: filepath.Join(t.TempDir(), "delta")}

	if got := d.load(); got != "" {
		t.Errorf("load of missing store = %q, want empty", got)
	}
	if err := d.save("  https://graph.microsoft.com/v1.0/drives/x/root/delta?token=abc \n"); err != nil {
		t.Fatal(err)
	}
	if got := d.load(); got != "https://graph.microsoft.com/v1.0/drives/x/root/delta?token=abc" {
		t.Errorf("load = %q (want trimmed link)", got)
	}
}
