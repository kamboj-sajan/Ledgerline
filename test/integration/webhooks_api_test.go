//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/kamboj-sajan/ledgerline/internal/payments"
	"github.com/kamboj-sajan/ledgerline/internal/webhooks"
)

type endpointResp struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	Secret string `json:"secret"`
}

func (e *apiEnv) createEndpoint(url string) endpointResp {
	e.t.Helper()
	r := e.post("/v1/webhook_endpoints", `{"url": "`+url+`"}`)
	wantStatus(e.t, r, 201)
	return decode[endpointResp](e.t, r)
}

func TestWebhookEndpointsAndOutbox(t *testing.T) {
	e := newAPIEnv(t)

	// An event from before any endpoint exists is not queued for delivery.
	early := e.createPayment("fp_ok", 1_000).ID

	ep := e.createEndpoint("https://merchant.example/hooks")
	if ep.Secret == "" || ep.ID == "" {
		t.Fatalf("endpoint = %+v, want an ID and a secret", ep)
	}
	wantError(t, e.post("/v1/webhook_endpoints", `{"url": "ftp://x"}`), 400, "parameter_invalid")
	wantError(t, e.post("/v1/webhook_endpoints", `{}`), 400, "parameter_missing")

	pid := e.paymentIn(payments.StatusCaptured) // created, authorized, captured
	if n := e.count(`SELECT count(*) FROM webhook_deliveries d JOIN events ev ON ev.id = d.event_id WHERE ev.payment_id = $1 AND d.endpoint_id = $2 AND d.status = 'pending'`, pid, ep.ID); n != 3 {
		t.Fatalf("pending deliveries for the new payment = %d, want 3", n)
	}
	if n := e.count(`SELECT count(*) FROM webhook_deliveries d JOIN events ev ON ev.id = d.event_id WHERE ev.payment_id = $1`, early); n != 0 {
		t.Fatalf("deliveries for an event that predates the endpoint = %d, want 0", n)
	}
	// A rejected operation is not a state change: no event, no delivery.
	before := e.count(`SELECT count(*) FROM events`)
	wantError(t, e.post("/v1/payments/"+pid+"/capture", ""), 409, "invalid_state_transition")
	if after := e.count(`SELECT count(*) FROM events`); after != before {
		t.Fatalf("events %d → %d after a rejected capture", before, after)
	}

	// Listing, oldest first, with a cursor.
	page1 := decode[struct {
		Data []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"data"`
	}](t, e.do("GET", "/v1/events?limit=2", "", ""))
	if len(page1.Data) != 2 || page1.Data[0].Type != "payment.created" {
		t.Fatalf("page 1 = %+v", page1)
	}
	page2 := decode[struct {
		Data []struct{ ID string } `json:"data"`
	}](t, e.do("GET", "/v1/events?limit=100&starting_after="+page1.Data[1].ID, "", ""))
	if len(page2.Data) != 2 { // 4 events in total: created (early), created, authorized, captured
		t.Fatalf("page 2 has %d events, want 2", len(page2.Data))
	}
	wantError(t, e.do("GET", "/v1/events?limit=0", "", ""), 400, "parameter_invalid")

	// Redeliver the early event: it gets a delivery to the endpoint added since.
	ev := page1.Data[0].ID
	r := e.post("/v1/events/"+ev+"/redeliver", "")
	wantStatus(t, r, 200)
	if got := decode[struct {
		Queued int `json:"deliveries_queued"`
	}](t, r); got.Queued != 1 {
		t.Fatalf("redelivery queued %d, want 1", got.Queued)
	}
	// Redelivering a dead delivery resets it with a fresh attempt budget.
	mustExec(t, e.db, `UPDATE webhook_deliveries SET status = 'dead', attempts = 10 WHERE event_id = $1`, ev)
	wantStatus(t, e.post("/v1/events/"+ev+"/redeliver", ""), 200)
	if n := e.count(`SELECT count(*) FROM webhook_deliveries WHERE event_id = $1 AND status = 'pending' AND attempts = 0`, ev); n != 1 {
		t.Fatalf("redelivered dead delivery not reset")
	}
	wantError(t, e.post("/v1/events/evt_missing/redeliver", ""), 404, "resource_missing")
	wantStatus(t, e.do("GET", "/v1/events/"+ev, "", ""), 200)

	// Rotation: a new secret, the old one remembered for the grace period.
	rot := decode[endpointResp](t, e.post("/v1/webhook_endpoints/"+ep.ID+"/rotate_secret", ""))
	if rot.Secret == "" || rot.Secret == ep.Secret {
		t.Fatalf("rotated secret = %q", rot.Secret)
	}
	if n := e.count(`SELECT count(*) FROM webhook_endpoints WHERE id = $1 AND secret = $2 AND previous_secret = $3 AND previous_secret_expires_at > now()`, ep.ID, rot.Secret, ep.Secret); n != 1 {
		t.Fatal("previous secret not kept after rotation")
	}
	wantError(t, e.post("/v1/webhook_endpoints/we_missing/rotate_secret", ""), 404, "resource_missing")

	// Another merchant sees none of this.
	_, otherKey := createMerchant(t, e)
	wantError(t, e.doWith(otherKey, "GET", "/v1/events/"+ev, "", ""), 404, "resource_missing")
	wantError(t, e.doWith(otherKey, "POST", "/v1/webhook_endpoints/"+ep.ID+"/rotate_secret", newKey(), ""), 404, "resource_missing")
}

// TestOutboxEventOnlyWithCommit: an event and its deliveries exist exactly
// when the transaction that wrote them commits.
func TestOutboxEventOnlyWithCommit(t *testing.T) {
	e := newAPIEnv(t)
	e.createEndpoint("https://merchant.example/hooks")
	ctx := t.Context()

	tx, err := e.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := webhooks.Insert(ctx, tx, e.merchantID, webhooks.EventPaymentCreated, "", map[string]string{"x": "y"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM events`) + e.count(`SELECT count(*) FROM webhook_deliveries`); n != 0 {
		t.Fatalf("%d outbox rows after rollback, want 0", n)
	}

	if err := withTx(t, e.db, func(ctx context.Context, tx pgx.Tx) error {
		_, err := webhooks.Insert(ctx, tx, e.merchantID, webhooks.EventPaymentCreated, "", map[string]string{"x": "y"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if ev, d := e.count(`SELECT count(*) FROM events`), e.count(`SELECT count(*) FROM webhook_deliveries`); ev != 1 || d != 1 {
		t.Fatalf("after commit: %d events, %d deliveries; want 1 and 1", ev, d)
	}
	for _, sql := range []string{`UPDATE events SET type = 'x'`, `DELETE FROM events`} {
		if _, err := e.db.Exec(ctx, sql); constraintOf(err) != "events_append_only" {
			t.Errorf("%s: %v, want events_append_only", sql, err)
		}
	}
}
