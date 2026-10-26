// Package payment is a fake card processor behind HTTP, instrumented with otelhttp.
package payment

import (
	"encoding/json"
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// ChargeRequest is the body of POST /charge.
type ChargeRequest struct {
	Card        string `json:"card"`
	AmountCents int64  `json:"amount_cents"`
}

// ChargeResponse is returned for approved and declined charges.
type ChargeResponse struct {
	Approved bool   `json:"approved"`
	Reason   string `json:"reason,omitempty"`
}

// Handler serves POST /charge. Cards ending in 0002 are declined, as are charges over the limit.
func Handler(tp trace.TracerProvider, limitCents int64) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /charge", func(w http.ResponseWriter, r *http.Request) {
		span := trace.SpanFromContext(r.Context())
		span.SetAttributes(attribute.String("http.route", "/charge"))
		var req ChargeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AmountCents <= 0 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		// Only ever record the last four digits; the full card number must never reach telemetry.
		last4 := req.Card
		if len(last4) > 4 {
			last4 = last4[len(last4)-4:]
		}
		span.SetAttributes(attribute.String("payment.card.last4", last4), attribute.Int64("payment.amount_cents", req.AmountCents))
		res := ChargeResponse{Approved: true}
		switch {
		case strings.HasSuffix(req.Card, "0002"):
			res = ChargeResponse{Reason: "card_declined"}
		case req.AmountCents > limitCents:
			res = ChargeResponse{Reason: "over_limit"}
		}
		outcome := "approved"
		if !res.Approved {
			outcome = res.Reason
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusPaymentRequired)
		}
		span.SetAttributes(attribute.String("payment.outcome", outcome))
		_ = json.NewEncoder(w).Encode(res)
	})
	return otelhttp.NewHandler(mux, "payment", otelhttp.WithTracerProvider(tp),
		otelhttp.WithSpanNameFormatter(spanName))
}

// spanName follows the HTTP semantic conventions: "{method} {route}". ServeMux patterns include
// the method ("POST /checkout"), so it is stripped from the route part.
func spanName(_ string, r *http.Request) string {
	route := r.Pattern
	if i := strings.IndexByte(route, ' '); i >= 0 {
		route = route[i+1:]
	}
	if route == "" {
		return r.Method
	}
	return r.Method + " " + route
}
