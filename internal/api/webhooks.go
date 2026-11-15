package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kamboj-sajan/ledgerline/internal/merchants"
	"github.com/kamboj-sajan/ledgerline/internal/webhooks"
)

type endpointJSON struct {
	ID        string    `json:"id"`
	Object    string    `json:"object"`
	URL       string    `json:"url"`
	Secret    string    `json:"secret,omitempty"` // only in the create and rotate responses
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

func toEndpointJSON(e webhooks.Endpoint) endpointJSON {
	return endpointJSON{ID: e.ID, Object: "webhook_endpoint", URL: e.URL, Secret: e.Secret, Enabled: e.Enabled, CreatedAt: e.CreatedAt.UTC()}
}

// createWebhookEndpoint registers a URL and returns its signing secret. The
// secret appears in this response only (and in its idempotent replay within
// 24 hours, which only the same merchant can request).
func (s *Server) createWebhookEndpoint(w http.ResponseWriter, r *http.Request, m merchants.Merchant) {
	body, apiErr := readBody(w, r)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if apiErr := decodeStrict(body, &req); apiErr != nil {
		writeError(w, apiErr)
		return
	}
	if req.URL == "" {
		writeError(w, paramMissing("url"))
		return
	}
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(req.URL) > 2048 {
		writeError(w, paramInvalid("url", "url must be an absolute http or https URL of at most 2048 characters"))
		return
	}
	s.runIdempotent(w, r, m, body, func(ctx context.Context, tx pgx.Tx) (int, any, error) {
		e, err := webhooks.CreateEndpoint(ctx, tx, m.ID, req.URL)
		if err != nil {
			return 0, nil, err
		}
		return http.StatusCreated, toEndpointJSON(e), nil
	})
}

func (s *Server) rotateWebhookSecret(w http.ResponseWriter, r *http.Request, m merchants.Merchant) {
	body, apiErr := readBody(w, r)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	if apiErr := decodeStrict(body, &struct{}{}); apiErr != nil {
		writeError(w, apiErr)
		return
	}
	s.runIdempotent(w, r, m, body, func(ctx context.Context, tx pgx.Tx) (int, any, error) {
		e, err := webhooks.RotateSecret(ctx, tx, m.ID, r.PathValue("id"), webhooks.RotationGrace)
		if errors.Is(err, webhooks.ErrNotFound) {
			return 0, nil, errNotFound
		}
		if err != nil {
			return 0, nil, err
		}
		return http.StatusOK, toEndpointJSON(e), nil
	})
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request, m merchants.Merchant) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeError(w, paramInvalid("limit", "limit must be between 1 and 100"))
			return
		}
		limit = n
	}
	events, err := webhooks.ListEvents(r.Context(), s.db, m.ID, r.URL.Query().Get("starting_after"), limit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := listJSON[json.RawMessage]{Object: "list", Data: []json.RawMessage{}}
	for _, e := range events {
		out.Data = append(out.Data, webhooks.EventJSON(e))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getEvent(w http.ResponseWriter, r *http.Request, m merchants.Merchant) {
	e, err := webhooks.GetEvent(r.Context(), s.db, m.ID, r.PathValue("id"))
	if errors.Is(err, webhooks.ErrNotFound) {
		writeError(w, errNotFound)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, webhooks.EventJSON(e))
}

// redeliverEvent queues an event again (for example after its deliveries
// went dead), with a fresh attempt budget.
func (s *Server) redeliverEvent(w http.ResponseWriter, r *http.Request, m merchants.Merchant) {
	body, apiErr := readBody(w, r)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	if apiErr := decodeStrict(body, &struct{}{}); apiErr != nil {
		writeError(w, apiErr)
		return
	}
	s.runIdempotent(w, r, m, body, func(ctx context.Context, tx pgx.Tx) (int, any, error) {
		n, err := webhooks.Redeliver(ctx, tx, m.ID, r.PathValue("id"))
		if errors.Is(err, webhooks.ErrNotFound) {
			return 0, nil, errNotFound
		}
		if err != nil {
			return 0, nil, err
		}
		return http.StatusOK, map[string]any{"object": "redelivery", "event_id": r.PathValue("id"), "deliveries_queued": n}, nil
	})
}
