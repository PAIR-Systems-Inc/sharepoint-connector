package smb

import (
	"context"
	"errors"
	"fmt"

	"github.com/cloudsoda/go-smb2"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/source"
)

// SMB2 CHANGE_NOTIFY turns the poll into an event: instead of walking the tree
// every couple of minutes to find modified timestamps, the server tells us when
// something changed.
//
// It does not replace the walk, and cannot. Three things still force a rescan:
// the server drops records when more changes arrive than its buffer can describe
// (ErrNotifyEnumDir), the watch dies with its handle if the session drops, and a
// notification only says *that* something changed — the connector still reads
// the file to sync it. So a watcher lowers latency; the periodic loops remain
// the guarantee.
//
// What it adds beyond speed is **deletions**. A modification-time walk cannot see
// them: a deleted file is simply absent, indistinguishable from unchanged. So
// deletions today wait for the periodic full sync. CHANGE_NOTIFY reports them
// explicitly, and this watcher turns them into an immediate reconcile request.

// deletionActions are the change kinds a timestamp walk cannot detect, so seeing
// one means a delta sync is not enough — the space must be reconciled against
// the share.
var deletionActions = map[uint32]bool{
	smb2.ActionRemoved:         true,
	smb2.ActionRenamedOldName:  true, // the old path is gone
	smb2.ActionRemovedByDelete: true,
}

// WatchChanges implements source.ChangeWatcher.
//
// It blocks until the share reports a change. Any change asks for a delta sync;
// a deletion or a lost notification stream additionally asks for a full
// reconcile, since neither is visible to a timestamp comparison.
func (a *Adapter) WatchChanges(ctx context.Context) (source.WatchResult, error) {
	notes, err := a.c.Watch(ctx)
	if err != nil {
		// The stream overflowed: changes happened but the server could not name
		// them. That is not a failure — it is precisely the case the reconcile
		// exists for.
		if errors.Is(err, smb2.ErrNotifyEnumDir) {
			return source.WatchResult{Changed: true, ReconcileRecommended: true}, nil
		}
		return source.WatchResult{}, err
	}

	return classifyNotifications(notes), nil
}

// classifyNotifications decides what a batch of change records means for the
// engine: any record is worth a delta, and a record a timestamp walk could not
// have seen is worth a full reconcile.
func classifyNotifications(notes []smb2.FileNotification) source.WatchResult {
	res := source.WatchResult{Changed: len(notes) > 0}
	for _, n := range notes {
		if deletionActions[n.Action] {
			res.ReconcileRecommended = true
			break
		}
	}
	return res
}

// Watch waits for one batch of change notifications on the sync root.
//
// The directory handle is opened per call rather than held open. A watch is
// bound to its handle, so a reconnect invalidates it anyway; reopening keeps the
// lifetime obvious and avoids holding a handle open on a customer's file server
// between events.
func (c *Client) Watch(ctx context.Context) ([]smb2.FileNotification, error) {
	w, ok := c.m.(watchMounter)
	if !ok {
		return nil, ErrWatchUnsupported
	}
	share, err := w.watchShare(ctx)
	if err != nil {
		return nil, err
	}

	dir, err := share.Open(walkRoot(c.cfg.Root))
	if err != nil {
		if isConnectionError(err) {
			c.m.invalidate()
		}
		return nil, fmt.Errorf("open watch dir: %w", err)
	}
	defer dir.Close()

	notes, err := dir.Notify(smb2.NotifyOptions{
		// Recursive watching on a handle opened at the *share root* is not
		// honored by Windows (the request is accepted and never completes),
		// though it works on a named subdirectory and against Samba. Recursion
		// is therefore only requested when SMB_ROOT names a subdirectory; at the
		// share root we watch shallowly and rely on the periodic reconcile for
		// deeper changes. Tracked upstream in CloudSoda/go-smb2#64.
		Recursive: walkRoot(c.cfg.Root) != ".",
	})
	if err != nil {
		if isConnectionError(err) {
			c.m.invalidate()
		}
		return nil, err
	}
	return notes, nil
}

// ErrWatchUnsupported means this client cannot watch — it was built over a
// caller-supplied filesystem (the tests) rather than a live SMB session.
var ErrWatchUnsupported = errors.New("smb: change notification unavailable on this client")

// watchMounter is the optional mounter capability that exposes the live share
// needed for CHANGE_NOTIFY. The in-memory mounter used by unit tests does not
// implement it, which is what keeps those tests server-free.
type watchMounter interface {
	watchShare(ctx context.Context) (*smb2.Share, error)
}

// watchShare returns the mounted share, dialing if necessary.
func (m *smbMounter) watchShare(ctx context.Context) (*smb2.Share, error) {
	if _, err := m.mount(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.share == nil {
		return nil, ErrWatchUnsupported
	}
	return m.share, nil
}

// WatchSupported reports whether this client can receive change notifications.
func (c *Client) WatchSupported() bool {
	_, ok := c.m.(watchMounter)
	return ok
}
