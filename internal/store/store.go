// Package store is an in-memory inventory that emits database client spans following the
// OpenTelemetry database semantic conventions, as a real SQL client library would.
package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Inventory holds stock per SKU.
type Inventory struct {
	tracer  trace.Tracer
	mu      sync.Mutex
	stock   map[string]int
	latency time.Duration
}

// New returns an inventory with the given stock. latency simulates a database round trip.
func New(tp trace.TracerProvider, stock map[string]int, latency time.Duration) *Inventory {
	cp := make(map[string]int, len(stock))
	for k, v := range stock {
		cp[k] = v
	}
	return &Inventory{tracer: tp.Tracer("store"), stock: cp, latency: latency}
}

func (i *Inventory) span(ctx context.Context, query string, rows int) (context.Context, trace.Span) {
	return i.tracer.Start(ctx, "SELECT inventory", trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(
		attribute.String("db.system.name", "postgresql"),
		attribute.String("db.operation.name", "SELECT"),
		attribute.String("db.collection.name", "inventory"),
		attribute.String("db.query.text", query),
		attribute.Int("db.response.returned_rows", rows),
	))
}

// Stock returns the stock for one SKU: one query per call.
func (i *Inventory) Stock(ctx context.Context, sku string) int {
	_, span := i.span(ctx, "SELECT sku, qty FROM inventory WHERE sku = $1", 1)
	defer span.End()
	time.Sleep(i.latency)
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.stock[sku]
}

// StockMany returns stock for several SKUs in a single query.
func (i *Inventory) StockMany(ctx context.Context, skus []string) map[string]int {
	params := make([]string, len(skus))
	for n := range skus {
		params[n] = fmt.Sprintf("$%d", n+1)
	}
	_, span := i.span(ctx, "SELECT sku, qty FROM inventory WHERE sku IN ("+strings.Join(params, ", ")+")", len(skus))
	defer span.End()
	time.Sleep(i.latency)
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make(map[string]int, len(skus))
	for _, s := range skus {
		out[s] = i.stock[s]
	}
	return out
}
