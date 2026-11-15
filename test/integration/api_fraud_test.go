//go:build integration

package integration

import (
	"bytes"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kamboj-sajan/ledgerline/internal/api"
	"github.com/kamboj-sajan/ledgerline/internal/fraud"
)

type fraudResp struct {
	Status      string  `json:"status"`
	FailureCode *string `json:"failure_code"`
	Fraud       *struct {
		Decision string   `json:"decision"`
		Rules    []string `json:"rules"`
	} `json:"fraud"`
}

func fraudEnv(t *testing.T) *apiEnv {
	t.Helper()
	var rules fraud.Config
	rules.HistoryWindow = fraud.Duration{Duration: 24 * time.Hour}
	rules.Velocity.Window = fraud.Duration{Duration: time.Hour}
	rules.Velocity.MaxCount = 3
	rules.AmountAbsolute.Max = map[string]int64{"USD": 50_000}
	rules.AmountAbsolute.Action = fraud.Block
	rules.CountryMismatch.Enabled = true
	if err := rules.Validate(); err != nil {
		t.Fatal(err)
	}
	return newAPIEnv(t, withConfig(api.Config{AuthorizeLockTimeout: 30 * time.Second, AcquirerTimeout: 5 * time.Second, Fraud: rules}))
}

func (e *apiEnv) createWith(card, country, ipCountry string, amount int64) string {
	e.t.Helper()
	body := `{"amount": ` + itoa(amount) + `, "currency": "USD", "customer_id": "cus", "card": {"fingerprint": "` + card +
		`", "country": "` + country + `"}, "ip_country": "` + ipCountry + `"}`
	r := e.post("/v1/payments", body)
	wantStatus(e.t, r, http.StatusCreated)
	return decode[paymentResp](e.t, r).ID
}

func TestFraudBlockFailsPaymentWithoutCallingAcquirer(t *testing.T) {
	e := fraudEnv(t)
	pid := e.createWith("fp_big", "US", "US", 50_001)
	key := newKey()
	first := e.do("POST", "/v1/payments/"+pid+"/authorize", key, "")
	er := wantError(t, first, http.StatusPaymentRequired, "fraud_suspected")
	if er.Error.Type != "card_error" || !strings.Contains(er.Error.Message, "amount_absolute") {
		t.Fatalf("error = %+v", er.Error)
	}
	got := decode[fraudResp](t, e.do("GET", "/v1/payments/"+pid, "", ""))
	if got.Status != "failed" || got.FailureCode == nil || *got.FailureCode != "fraud_suspected" ||
		got.Fraud == nil || got.Fraud.Decision != "block" || !slices.Equal(got.Fraud.Rules, []string{"amount_absolute"}) {
		t.Fatalf("payment = %+v (fraud %+v)", got, got.Fraud)
	}
	if n := e.acquirerAuthorizations(pid); n != 0 {
		t.Fatalf("acquirer authorizations = %d, want 0: a blocked payment must never reach the network", n)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE payment_id = $1 AND type = 'payment.failed'`, pid); n != 1 {
		t.Fatalf("payment.failed events = %d, want 1", n)
	}
	if again := e.do("POST", "/v1/payments/"+pid+"/authorize", key, ""); !bytes.Equal(again.Body, first.Body) {
		t.Fatalf("replay = %s", again)
	}
	e.mustAudit()
}

func TestFraudReviewStillAuthorizes(t *testing.T) {
	e := fraudEnv(t)
	pid := e.createWith("fp_traveller", "US", "RU", 1_000)
	r := e.post("/v1/payments/"+pid+"/authorize", "")
	wantStatus(t, r, http.StatusOK)
	got := decode[fraudResp](t, r)
	if got.Status != "authorized" || got.Fraud == nil || got.Fraud.Decision != "review" ||
		!slices.Equal(got.Fraud.Rules, []string{"geo_country_mismatch"}) {
		t.Fatalf("payment = %+v (fraud %+v)", got, got.Fraud)
	}
	if n := e.acquirerAuthorizations(pid); n != 1 {
		t.Fatalf("acquirer authorizations = %d, want 1", n)
	}
}

func TestFraudAllowRecordsDecision(t *testing.T) {
	e := fraudEnv(t)
	pid := e.createWith("fp_clean", "US", "US", 1_000)
	if got := decode[fraudResp](t, e.do("GET", "/v1/payments/"+pid, "", "")); got.Fraud != nil {
		t.Fatalf("fraud = %+v before authorization, want null", got.Fraud)
	}
	got := decode[fraudResp](t, e.post("/v1/payments/"+pid+"/authorize", ""))
	if got.Fraud == nil || got.Fraud.Decision != "allow" || len(got.Fraud.Rules) != 0 {
		t.Fatalf("fraud = %+v, want allow with no rules", got.Fraud)
	}
}

// TestFraudVelocityAcrossMerchants: velocity is about the card, so payments
// at another merchant count too.
func TestFraudVelocityAcrossMerchants(t *testing.T) {
	e := fraudEnv(t)
	_, otherKey := createMerchant(t, e)
	body := `{"amount": 500, "currency": "USD", "customer_id": "c", "card": {"fingerprint": "fp_busy", "country": "US"}, "ip_country": "US"}`
	for range 2 {
		wantStatus(t, e.doWith(otherKey, "POST", "/v1/payments", newKey(), body), http.StatusCreated)
	}
	first := e.createWith("fp_busy", "US", "US", 500) // third for this card
	if got := decode[fraudResp](t, e.post("/v1/payments/"+first+"/authorize", "")); got.Fraud.Decision != "allow" {
		t.Fatalf("3rd payment in an hour (limit 3) = %+v", got.Fraud)
	}
	fourth := e.createWith("fp_busy", "US", "US", 500)
	got := decode[fraudResp](t, e.post("/v1/payments/"+fourth+"/authorize", ""))
	if got.Fraud.Decision != "review" || !slices.Equal(got.Fraud.Rules, []string{"velocity_count"}) {
		t.Fatalf("4th payment in an hour = %+v, want review [velocity_count]", got.Fraud)
	}
}
