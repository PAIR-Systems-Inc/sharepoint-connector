package googledrive

import (
	"encoding/json"
	"errors"
	"os"
)

// ChannelStore persists the current push channel (its id + resourceId) so a
// restart can stop the previously-created channel before opening a new one.
// Google Drive can neither renew a channel in place nor stop one it can't
// identify, so without durable state every process restart orphans the live
// channel (it keeps delivering duplicate — harmless, coalesced — pings until it
// expires, and Google carries a leaked active channel per restart).
type ChannelStore interface {
	// Load returns the last saved channel pair, or ("","",nil) if none is stored.
	Load() (id, resourceID string, err error)
	// Save records the current channel pair.
	Save(id, resourceID string) error
}

// FileChannelStore persists the channel pair as JSON at Path — placed alongside
// the delta cursor on the durable volume so it survives restarts.
type FileChannelStore struct{ Path string }

type channelState struct {
	ID         string `json:"id"`
	ResourceID string `json:"resource_id"`
}

func (s FileChannelStore) Load() (string, string, error) {
	b, err := os.ReadFile(s.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", nil // nothing stored yet
		}
		return "", "", err
	}
	var v channelState
	if err := json.Unmarshal(b, &v); err != nil {
		return "", "", err
	}
	return v.ID, v.ResourceID, nil
}

func (s FileChannelStore) Save(id, resourceID string) error {
	b, err := json.Marshal(channelState{ID: id, ResourceID: resourceID})
	if err != nil {
		return err
	}
	return os.WriteFile(s.Path, b, 0o600)
}
