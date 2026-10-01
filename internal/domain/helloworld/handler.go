package helloworld

import (
	"context"
	"net/http"

	"github.com/smallnest/matex/pkg/core/errs"
	"github.com/smallnest/matex/pkg/core/httpx"
)

// errNoDB is returned when a route needs the database but none is
// configured.
var errNoDB = errs.Unavailable(50301, "database is not configured")

// Handler is the HTTP edge of the domain: parse input, call the
// service, return the payload (or an errs error). No framing concerns.
type Handler struct {
	svc *Service
}

// NewHandler builds a Handler.
func NewHandler(s *Service) *Handler { return &Handler{svc: s} }

// Greet handles GET /api/v1/hello/{name}.
func (h *Handler) Greet(ctx context.Context, r *http.Request) (any, error) {
	name := r.PathValue("name")
	if name == "" {
		return nil, errs.Invalid(40001, "name is required")
	}
	return h.svc.Greet(ctx, name)
}

// Stats handles GET /api/v1/hello/stats/{name}.
func (h *Handler) Stats(ctx context.Context, r *http.Request) (any, error) {
	name := r.PathValue("name")
	if name == "" {
		return nil, errs.Invalid(40001, "name is required")
	}
	return h.svc.Stats(ctx, name)
}

// publishEventRequest is the body of the event endpoint.
type publishEventRequest struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

// PublishEvent handles POST /api/v1/hello/events: publish a kafka
// record synchronously (requires a configured broker).
func (h *Handler) PublishEvent(ctx context.Context, r *http.Request) (any, error) {
	var req publishEventRequest
	if err := httpx.ReadJSON(r, &req); err != nil {
		return nil, errs.Invalid(40002, "bad request body: %v", err)
	}
	if req.Name == "" {
		return nil, errs.Invalid(40001, "name is required")
	}
	if h.svc.deps.Producer == nil {
		return nil, errs.Unavailable(50302, "kafka is not configured")
	}
	if err := h.svc.deps.Producer.SendJSON(ctx, TopicGreetings, req.Name, req); err != nil {
		return nil, errs.InternalWrap(50001, err, "publish failed")
	}
	return map[string]string{"status": "published", "name": req.Name}, nil
}
