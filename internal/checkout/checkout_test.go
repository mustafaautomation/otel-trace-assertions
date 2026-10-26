package checkout_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/mustafaautomation/otel-trace-assertions/internal/checkout"
	"github.com/mustafaautomation/otel-trace-assertions/internal/payment"
	"github.com/mustafaautomation/otel-trace-assertions/internal/store"
	"github.com/mustafaautomation/otel-trace-assertions/traceassert"
)

const card = "4111111111111111"

func init() {
	// W3C trace context, so the payment service joins the caller's trace.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
}

type env struct {
	checkoutURL string
	checkoutRec *tracetest.SpanRecorder
	paymentRec  *tracetest.SpanRecorder
	payment     *httptest.Server
}

func provider(t *testing.T, service string) (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	res := resource.NewSchemaless()
	res, err := resource.Merge(res, resource.NewSchemaless(serviceName(service)))
	if err != nil {
		t.Fatal(err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec), sdktrace.WithResource(res))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	return tp, rec
}

func start(t *testing.T, batch bool) *env {
	t.Helper()
	payTP, payRec := provider(t, "payment")
	pay := httptest.NewServer(payment.Handler(payTP, 50_000))
	t.Cleanup(pay.Close)

	coTP, coRec := provider(t, "checkout")
	inv := store.New(coTP, map[string]int{"SKU-1": 10, "SKU-2": 5, "SKU-3": 1, "SKU-4": 8, "SKU-5": 3}, time.Millisecond)
	co := httptest.NewServer(checkout.Handler(checkout.Config{
		Tracer: coTP, Inventory: inv, PaymentURL: pay.URL, BatchStockQuery: batch,
	}))
	t.Cleanup(co.Close)
	return &env{checkoutURL: co.URL, checkoutRec: coRec, paymentRec: payRec, payment: pay}
}

func (e *env) post(t *testing.T, o checkout.Order) int {
	t.Helper()
	body, _ := json.Marshal(o)
	resp, err := http.Post(e.checkoutURL+"/checkout", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// trace waits until the server spans have ended (otelhttp ends them after the response is
// written) and returns every span from both services.
func (e *env) trace(t *testing.T) *traceassert.Trace {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hasServerSpan(e.checkoutRec.Ended()) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	return traceassert.New(t, e.checkoutRec.Ended(), e.paymentRec.Ended())
}

func hasServerSpan(spans []sdktrace.ReadOnlySpan) bool {
	for _, s := range spans {
		if s.SpanKind() == trace.SpanKindServer {
			return true
		}
	}
	return false
}

func order(items ...checkout.Item) checkout.Order {
	return checkout.Order{CustomerID: "cust-42", Card: card, Items: items}
}

var twoItems = []checkout.Item{{SKU: "SKU-1", Qty: 2, PriceCts: 1500}, {SKU: "SKU-2", Qty: 1, PriceCts: 999}}

func TestHappyPathTraceShape(t *testing.T) {
	e := start(t, true)
	if got := e.post(t, order(twoItems...)); got != http.StatusOK {
		t.Fatalf("status %d", got)
	}
	tr := e.trace(t)

	server := tr.Named("POST /checkout").One().Kind(trace.SpanKindServer).
		Attr("http.request.method", "POST").
		Attr("http.response.status_code", 200).
		Attr("http.route", "/checkout").
		Attr("app.order.items", 2).
		Status(codes.Unset)
	tr.Named("validate order").One().ChildOf(server).Attr("app.order.total_cents", 3999)
	stock := tr.Named("check stock").One().ChildOf(server)
	tr.Named("SELECT inventory").Count(1).ChildOf(stock).Kind(trace.SpanKindClient).
		Attr("db.system.name", "postgresql").
		Attr("db.query.text", "SELECT sku, qty FROM inventory WHERE sku IN ($1, $2)")
	charge := tr.Named("charge payment").One().ChildOf(server).Status(codes.Unset)
	client := tr.Where("payment HTTP client", func(s sdktrace.ReadOnlySpan) bool {
		return s.SpanKind() == trace.SpanKindClient && traceassert.ServiceName(s) == "checkout" && s.Name() != "SELECT inventory"
	}).One().ChildOf(charge).Attr("http.response.status_code", 200)

	// The payment service's server span joined the same trace, under the client span.
	tr.Service("payment").One().Kind(trace.SpanKindServer).ChildOf(client).Attr("http.route", "/charge").
		Attr("payment.outcome", "approved").Attr("payment.card.last4", "1111")
	tr.Where("all spans", func(sdktrace.ReadOnlySpan) bool { return true }).SameTrace()
}

func TestNoCardNumberInAnyTelemetry(t *testing.T) {
	e := start(t, true)
	e.post(t, order(twoItems...))
	tr := e.trace(t)
	for _, s := range tr.Spans() {
		for _, kv := range s.Attributes() {
			if strings.Contains(kv.Value.String(), card) {
				t.Errorf("span %q attribute %s contains the full card number", s.Name(), kv.Key)
			}
		}
	}
	tr.Where("all spans", func(sdktrace.ReadOnlySpan) bool { return true }).NoAttr("payment.card.number", "card")
}

func TestDeclinedPaymentIsAnErrorOnTheChargeSpanOnly(t *testing.T) {
	e := start(t, true)
	o := order(twoItems...)
	o.Card = "4000000000000002"
	if got := e.post(t, o); got != http.StatusPaymentRequired {
		t.Fatalf("status %d", got)
	}
	tr := e.trace(t)
	tr.Named("charge payment").One().Status(codes.Error).Event("exception").Attr("app.payment.decline_reason", "card_declined")
	// Semantic conventions: a 4xx is the client's problem, so server spans keep status Unset.
	tr.Named("POST /checkout").One().Attr("http.response.status_code", 402).Status(codes.Unset)
	tr.Service("payment").One().Attr("payment.outcome", "card_declined").Status(codes.Unset)
}

func TestPaymentOutageMarksServerSpanAsError(t *testing.T) {
	e := start(t, true)
	e.payment.Close()
	if got := e.post(t, order(twoItems...)); got != http.StatusBadGateway {
		t.Fatalf("status %d", got)
	}
	tr := e.trace(t)
	tr.Named("POST /checkout").One().Attr("http.response.status_code", 502).Status(codes.Error)
	tr.Named("charge payment").One().Status(codes.Error).Event("exception")
	tr.Service("payment").Count(0)
}

func TestOutOfStockStopsBeforePayment(t *testing.T) {
	e := start(t, true)
	if got := e.post(t, order(checkout.Item{SKU: "SKU-3", Qty: 2, PriceCts: 100})); got != http.StatusConflict {
		t.Fatalf("status %d", got)
	}
	tr := e.trace(t)
	tr.Named("check stock").One().Status(codes.Error).Event("out_of_stock")
	tr.Named("charge payment").Count(0)
	tr.Service("payment").Count(0)
}

func TestInvalidOrderMakesNoDatabaseCalls(t *testing.T) {
	e := start(t, true)
	if got := e.post(t, checkout.Order{CustomerID: "cust-42", Card: card}); got != http.StatusBadRequest {
		t.Fatalf("status %d", got)
	}
	tr := e.trace(t)
	tr.Named("validate order").One().Status(codes.Error)
	tr.Named("SELECT inventory").Count(0)
}

// The batched query keeps database calls flat however big the order is.
func TestStockQueriesDoNotGrowWithOrderSize(t *testing.T) {
	for _, n := range []int{1, 3, 5} {
		t.Run(fmt.Sprintf("%d items", n), func(t *testing.T) {
			e := start(t, true)
			var items []checkout.Item
			for i := 1; i <= n; i++ {
				items = append(items, checkout.Item{SKU: fmt.Sprintf("SKU-%d", i), Qty: 1, PriceCts: 100})
			}
			e.post(t, order(items...))
			e.trace(t).Named("SELECT inventory").AtMost(1)
		})
	}
}

// With batching off the service does one query per item. The same assertion must catch it.
func TestNPlusOneIsCaught(t *testing.T) {
	e := start(t, false)
	var items []checkout.Item
	for i := 1; i <= 5; i++ {
		items = append(items, checkout.Item{SKU: fmt.Sprintf("SKU-%d", i), Qty: 1, PriceCts: 100})
	}
	e.post(t, order(items...))
	waitForServer(t, e)
	fake := &recordingTB{}
	tr := traceassert.New(fake, e.checkoutRec.Ended(), e.paymentRec.Ended())
	tr.Named("SELECT inventory").AtMost(1)
	if len(fake.errors) != 1 || !strings.Contains(fake.errors[0], "want at most 1, got 5") {
		t.Fatalf("expected the N+1 guard to fail with 5 queries, got %q", fake.errors)
	}
	if !strings.Contains(fake.errors[0], "- POST /checkout [checkout, server]") {
		t.Errorf("failure message should include the trace tree:\n%s", fake.errors[0])
	}
}

func waitForServer(t *testing.T, e *env) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !hasServerSpan(e.checkoutRec.Ended()) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

type recordingTB struct{ errors []string }

func (r *recordingTB) Helper() {}
func (r *recordingTB) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}
