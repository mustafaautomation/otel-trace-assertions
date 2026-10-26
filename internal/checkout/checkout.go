// Package checkout is an HTTP service that checks stock and charges a card through the payment
// service. It is instrumented with OpenTelemetry so tests can assert on the traces it produces.
package checkout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/mustafaautomation/otel-trace-assertions/internal/payment"
	"github.com/mustafaautomation/otel-trace-assertions/internal/store"
)

// Item is one order line.
type Item struct {
	SKU      string `json:"sku"`
	Qty      int    `json:"qty"`
	PriceCts int64  `json:"price_cents"`
}

// Order is the body of POST /checkout.
type Order struct {
	CustomerID string `json:"customer_id"`
	Card       string `json:"card"`
	Items      []Item `json:"items"`
}

// Config wires the service.
type Config struct {
	Tracer      trace.TracerProvider
	Inventory   *store.Inventory
	PaymentURL  string
	PaymentHTTP *http.Client
	// BatchStockQuery uses one inventory query for all items. Turning it off reproduces the
	// classic N+1 pattern (one query per item), which the trace tests are there to catch.
	BatchStockQuery bool
}

type service struct {
	cfg    Config
	tracer trace.Tracer
}

// Handler returns the instrumented HTTP handler for POST /checkout.
func Handler(cfg Config) http.Handler {
	if cfg.PaymentHTTP == nil {
		cfg.PaymentHTTP = &http.Client{
			Timeout:   5 * time.Second,
			Transport: otelhttp.NewTransport(http.DefaultTransport, otelhttp.WithTracerProvider(cfg.Tracer)),
		}
	}
	s := &service{cfg: cfg, tracer: cfg.Tracer.Tracer("checkout")}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /checkout", withRoute(s.checkout))
	return otelhttp.NewHandler(mux, "checkout", otelhttp.WithTracerProvider(cfg.Tracer),
		otelhttp.WithSpanNameFormatter(spanName))
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func (s *service) checkout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var o Order
	if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
		http.Error(w, "malformed order", http.StatusBadRequest)
		return
	}
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.String("app.customer.id", o.CustomerID),
		attribute.Int("app.order.items", len(o.Items)),
	)
	total, err := s.validate(ctx, o)
	if err == nil {
		err = s.reserve(ctx, o)
	}
	if err == nil {
		err = s.charge(ctx, o.Card, total)
	}
	var he *httpError
	if errors.As(err, &he) {
		http.Error(w, he.msg, he.status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "confirmed", "total_cents": total})
}

func (s *service) validate(ctx context.Context, o Order) (int64, error) {
	_, span := s.tracer.Start(ctx, "validate order")
	defer span.End()
	if o.CustomerID == "" || len(o.Items) == 0 {
		span.SetStatus(codes.Error, "invalid order")
		return 0, &httpError{http.StatusBadRequest, "order needs a customer and at least one item"}
	}
	var total int64
	for _, it := range o.Items {
		if it.Qty <= 0 || it.PriceCts <= 0 {
			span.SetStatus(codes.Error, "invalid item")
			return 0, &httpError{http.StatusBadRequest, fmt.Sprintf("bad item %q", it.SKU)}
		}
		total += int64(it.Qty) * it.PriceCts
	}
	span.SetAttributes(attribute.Int64("app.order.total_cents", total))
	return total, nil
}

func (s *service) reserve(ctx context.Context, o Order) error {
	ctx, span := s.tracer.Start(ctx, "check stock")
	defer span.End()
	stock := map[string]int{}
	if s.cfg.BatchStockQuery {
		skus := make([]string, len(o.Items))
		for i, it := range o.Items {
			skus[i] = it.SKU
		}
		stock = s.cfg.Inventory.StockMany(ctx, skus)
	} else {
		for _, it := range o.Items {
			stock[it.SKU] = s.cfg.Inventory.Stock(ctx, it.SKU)
		}
	}
	for _, it := range o.Items {
		if stock[it.SKU] < it.Qty {
			span.SetStatus(codes.Error, "out of stock")
			span.AddEvent("out_of_stock", trace.WithAttributes(attribute.String("app.sku", it.SKU)))
			return &httpError{http.StatusConflict, fmt.Sprintf("%s is out of stock", it.SKU)}
		}
	}
	return nil
}

func (s *service) charge(ctx context.Context, card string, total int64) error {
	ctx, span := s.tracer.Start(ctx, "charge payment")
	defer span.End()
	body, _ := json.Marshal(payment.ChargeRequest{Card: card, AmountCents: total})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.PaymentURL+"/charge", bytes.NewReader(body))
	if err != nil {
		return s.fail(span, err, http.StatusInternalServerError, "cannot build payment request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.cfg.PaymentHTTP.Do(req)
	if err != nil {
		return s.fail(span, err, http.StatusBadGateway, "payment service unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	var res payment.ChargeResponse
	_ = json.NewDecoder(resp.Body).Decode(&res)
	if resp.StatusCode == http.StatusPaymentRequired {
		span.SetAttributes(attribute.String("app.payment.decline_reason", res.Reason))
		return s.fail(span, errors.New(res.Reason), http.StatusPaymentRequired, "payment declined")
	}
	if resp.StatusCode != http.StatusOK || !res.Approved {
		return s.fail(span, fmt.Errorf("payment status %d", resp.StatusCode), http.StatusBadGateway, "payment failed")
	}
	return nil
}

func (s *service) fail(span trace.Span, err error, status int, msg string) error {
	span.RecordError(err)
	span.SetStatus(codes.Error, msg)
	return &httpError{status, msg}
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

// withRoute records http.route on the server span. otelhttp wraps the whole mux, so it cannot know
// the matched route by itself; the trace tests caught it missing.
func withRoute(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		route := r.Pattern
		if i := strings.IndexByte(route, ' '); i >= 0 {
			route = route[i+1:]
		}
		trace.SpanFromContext(r.Context()).SetAttributes(attribute.String("http.route", route))
		h(w, r)
	}
}
