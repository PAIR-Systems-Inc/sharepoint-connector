package graph

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// driveItem subscription lifetime bounds (minutes).
const (
	SubMinutesDefault = 4320  // 3 days
	SubMinutesMin     = 45    // Graph minimum for driveItem
	SubMinutesMax     = 42300 // ~30 days, Graph maximum
)

// Subscription is a Microsoft Graph change-notification subscription.
type Subscription struct {
	ID                 string `json:"id"`
	Resource           string `json:"resource"`
	ChangeType         string `json:"changeType"`
	ClientState        string `json:"clientState"`
	NotificationURL    string `json:"notificationUrl"`
	ExpirationDateTime string `json:"expirationDateTime"`
}

// expirationString returns an ISO-8601 expiration `minutes` from now, capped at
// the Graph maximum (format matches the Python "…:…:…000Z").
func expirationString(minutes int) string {
	if minutes > SubMinutesMax {
		minutes = SubMinutesMax
	}
	return time.Now().UTC().Add(time.Duration(minutes) * time.Minute).Format("2006-01-02T15:04:05.000Z")
}

// ListSubscriptions returns the app's change-notification subscriptions.
func (c *Client) ListSubscriptions() ([]Subscription, error) {
	var data struct {
		Value []Subscription `json:"value"`
	}
	if err := c.getJSON(c.graphBase+"/subscriptions", &data); err != nil {
		return nil, err
	}
	return data.Value, nil
}

// RenewSubscription extends a subscription's expiration via PATCH.
func (c *Client) RenewSubscription(id, expiration string) (*Subscription, error) {
	var sub Subscription
	if err := c.reqJSON(http.MethodPatch, c.graphBase+"/subscriptions/"+id,
		map[string]any{"expirationDateTime": expiration}, &sub); err != nil {
		return nil, err
	}
	return &sub, nil
}

// EnsureSubscription creates the driveItem change subscription for the site's
// first drive root, or renews it if one already exists for the same resource and
// clientState. Ported from create_subscription in listener.py.
func (c *Client) EnsureSubscription(notificationURL, clientState string, expirationMinutes int) (*Subscription, error) {
	siteID, err := c.GetSiteID()
	if err != nil {
		return nil, err
	}
	drives, err := c.GetDrives(siteID)
	if err != nil {
		return nil, err
	}
	if len(drives) == 0 {
		return nil, errors.New("no drives found for site")
	}
	resource := fmt.Sprintf("sites/%s/drives/%s/root", siteID, drives[0].ID)
	expiration := expirationString(expirationMinutes)

	subs, err := c.ListSubscriptions()
	if err != nil {
		return nil, err
	}
	for _, s := range subs {
		if s.Resource == resource && s.ClientState == clientState {
			return c.RenewSubscription(s.ID, expiration)
		}
	}
	body := map[string]any{
		"changeType":                "updated",
		"notificationUrl":           notificationURL,
		"resource":                  resource,
		"expirationDateTime":        expiration,
		"clientState":               clientState,
		"latestSupportedTlsVersion": "v1_2",
	}
	var sub Subscription
	if err := c.reqJSON(http.MethodPost, c.graphBase+"/subscriptions", body, &sub); err != nil {
		return nil, err
	}
	return &sub, nil
}
