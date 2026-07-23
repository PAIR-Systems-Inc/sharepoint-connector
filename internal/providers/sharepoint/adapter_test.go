package sharepoint

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/source"
)

// TestAdapterValidateWebhook covers the Graph webhook contract now living in the
// provider: the validationToken handshake and the clientState spoof check.
func TestAdapterValidateWebhook(t *testing.T) {
	a := NewAdapter(nil, "", "secret") // client unused by ValidateWebhook

	// A ?validationToken=… query is echoed back (subscription-validation handshake).
	r := httptest.NewRequest(http.MethodPost, "/sync/webhook?validationToken=xyz", nil)
	if res, echo := a.ValidateWebhook(r, nil); res != source.WebhookHandshake || echo != "xyz" {
		t.Errorf("handshake: got (%v, %q), want (handshake, xyz)", res, echo)
	}

	post := httptest.NewRequest(http.MethodPost, "/sync/webhook", strings.NewReader(""))

	// Matching clientState → a genuine change.
	if res, _ := a.ValidateWebhook(post, []byte(`{"value":[{"clientState":"secret"}]}`)); res != source.WebhookChange {
		t.Errorf("valid notification: got %v, want change", res)
	}

	// Any mismatched clientState → reject (spoof protection).
	if res, _ := a.ValidateWebhook(post, []byte(`{"value":[{"clientState":"nope"}]}`)); res != source.WebhookReject {
		t.Errorf("spoofed clientState: got %v, want reject", res)
	}

	// A non-POST without a validationToken → reject.
	get := httptest.NewRequest(http.MethodGet, "/sync/webhook", nil)
	if res, _ := a.ValidateWebhook(get, nil); res != source.WebhookReject {
		t.Errorf("bare GET: got %v, want reject", res)
	}
}
