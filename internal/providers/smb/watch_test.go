package smb

import (
	"context"
	"errors"
	"testing"
	"testing/fstest"

	"github.com/cloudsoda/go-smb2"
)

// The engine's delta is a modification-time comparison, so it can see additions
// and edits but never a deletion — a deleted file is simply absent, which is
// indistinguishable from unchanged. Any notification implying something is gone
// must therefore escalate to a full reconcile.
func TestClassifyNotifications(t *testing.T) {
	cases := []struct {
		name          string
		notes         []smb2.FileNotification
		wantChanged   bool
		wantReconcile bool
	}{
		{"nothing", nil, false, false},
		{"added", []smb2.FileNotification{{Action: smb2.ActionAdded, Name: "a.txt"}}, true, false},
		{"modified", []smb2.FileNotification{{Action: smb2.ActionModified, Name: "a.txt"}}, true, false},
		{"removed escalates", []smb2.FileNotification{{Action: smb2.ActionRemoved, Name: "a.txt"}}, true, true},
		{"removed-by-delete escalates", []smb2.FileNotification{{Action: smb2.ActionRemovedByDelete, Name: "a.txt"}}, true, true},
		// The old name of a rename no longer exists, so its memory is orphaned.
		{"rename old-name escalates", []smb2.FileNotification{{Action: smb2.ActionRenamedOldName, Name: "a.txt"}}, true, true},
		// The new name is just an addition; on its own it needs no reconcile.
		{"rename new-name alone does not", []smb2.FileNotification{{Action: smb2.ActionRenamedNewName, Name: "b.txt"}}, true, false},
		{"a deletion anywhere in the batch escalates", []smb2.FileNotification{
			{Action: smb2.ActionAdded, Name: "a.txt"},
			{Action: smb2.ActionModified, Name: "b.txt"},
			{Action: smb2.ActionRemoved, Name: "c.txt"},
		}, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyNotifications(tc.notes)
			if got.Changed != tc.wantChanged {
				t.Errorf("Changed = %v, want %v", got.Changed, tc.wantChanged)
			}
			if got.ReconcileRecommended != tc.wantReconcile {
				t.Errorf("ReconcileRecommended = %v, want %v", got.ReconcileRecommended, tc.wantReconcile)
			}
		})
	}
}

// A client built over an in-memory filesystem has no SMB session, so it cannot
// watch. It must say so plainly rather than appear to watch and never fire.
func TestWatch_UnsupportedOnFakeClient(t *testing.T) {
	c := newWithFS(Config{Host: "h", Share: "s", User: "u"}, fstest.MapFS{})
	if c.WatchSupported() {
		t.Error("WatchSupported() = true for a fake client")
	}
	if _, err := c.Watch(context.Background()); !errors.Is(err, ErrWatchUnsupported) {
		t.Errorf("Watch() err = %v, want ErrWatchUnsupported", err)
	}
}

// A real client advertises the capability, so the listener's type assertion for
// source.ChangeWatcher picks it up.
func TestWatch_SupportedOnRealClient(t *testing.T) {
	c, err := New(Config{Host: "fs1", Share: "data", User: "u", Password: "p"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !c.WatchSupported() {
		t.Error("WatchSupported() = false for a live-session client")
	}
}
