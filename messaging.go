package billdogeng

import "context"

// Messaging dispatch (BillDog advantage over PostHog) — server-side delivery
// across push / email / sms / in-app / live-activity.
//
// Note: messaging-dispatch authenticates with a Supabase session Bearer JWT +
// project membership, not the X-BillDog-API-Key key used by the other modules. Pass the
// JWT as AccessToken per call.
type Messaging struct {
	transport *transport
}

func newMessaging(t *transport) *Messaging {
	return &Messaging{transport: t}
}

// DispatchResult is the response from Dispatch.
type DispatchResult = map[string]interface{}

// Dispatch dispatches a message. Returns {sent, failed} (or {success:false, ...}
// when no recipients match).
func (m *Messaging) Dispatch(ctx context.Context, params DispatchParams) (DispatchResult, error) {
	body := map[string]interface{}{
		"action":     "send",
		"project_id": params.ProjectID,
		"channel":    params.Channel,
		"content":    params.Content,
	}
	if params.Targeting != nil {
		body["targeting"] = params.Targeting
	}
	if params.Scheduling != nil {
		body["scheduling"] = params.Scheduling
	}
	if params.TemplateID != "" {
		body["template_id"] = params.TemplateID
	}

	var data DispatchResult
	err := m.transport.request(ctx, requestOptions{
		path:    "/messaging-dispatch",
		body:    body,
		headers: map[string]string{"Authorization": "Bearer " + params.AccessToken},
		noGzip:  true,
	}, &data)
	return data, err
}
