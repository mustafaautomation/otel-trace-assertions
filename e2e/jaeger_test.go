//go:build e2e

// Package e2e exports real traces over OTLP to a running Jaeger and asserts on what Jaeger stored,
// so the whole path (SDK, exporter, propagation, backend) is tested, not just in-memory spans.
//
//	docker run -d -p 16686:16686 -p 4318:4318 jaegertracing/jaeger:2.22.0
//	go test -tags e2e ./e2e/
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/mustafaautomation/otel-trace-assertions/internal/checkout"
	"github.com/mustafaautomation/otel-trace-assertions/internal/payment"
	"github.com/mustafaautomation/otel-trace-assertions/internal/store"
	"github.com/mustafaautomation/otel-trace-assertions/internal/telemetry"
)

type jaegerTrace struct {
	Data []struct {
		Spans []struct {
			SpanID        string `json:"spanID"`
			OperationName string `json:"operationName"`
			ProcessID     string `json:"processID"`
			References    []struct {
				RefType string `json:"refType"`
				SpanID  string `json:"spanID"`
			} `json:"references"`
			Tags []struct {
				Key   string `json:"key"`
				Value any    `json:"value"`
			} `json:"tags"`
		} `json:"spans"`
		Processes map[string]struct {
			ServiceName string `json:"serviceName"`
		} `json:"processes"`
	} `json:"data"`
}

func provider(t *testing.T, service string) *sdktrace.TracerProvider {
	t.Helper()
	tp, err := telemetry.OTLPProvider(t.Context(), service)
	if err != nil {
		t.Fatal(err)
	}
	return tp
}

func TestTraceArrivesInJaegerAcrossServices(t *testing.T) {
	jaeger := os.Getenv("JAEGER_QUERY_URL")
	if jaeger == "" {
		jaeger = "http://localhost:16686"
	}
	otel.SetTextMapPropagator(propagation.TraceContext{})

	clientTP, coTP, payTP := provider(t, "e2e-client"), provider(t, "checkout"), provider(t, "payment")
	pay := httptest.NewServer(payment.Handler(payTP, 50_000))
	defer pay.Close()
	inv := store.New(coTP, map[string]int{"SKU-1": 10}, time.Millisecond)
	co := httptest.NewServer(checkout.Handler(checkout.Config{Tracer: coTP, Inventory: inv, PaymentURL: pay.URL, BatchStockQuery: true}))
	defer co.Close()

	// The test is the first hop: it starts the trace, so it knows the trace ID to look up.
	ctx, root := clientTP.Tracer("e2e").Start(t.Context(), "place order")
	traceID := root.SpanContext().TraceID().String()
	body, _ := json.Marshal(checkout.Order{CustomerID: "cust-e2e", Card: "4111111111111111",
		Items: []checkout.Item{{SKU: "SKU-1", Qty: 1, PriceCts: 2500}}})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, co.URL+"/checkout", bytes.NewReader(body))
	client := &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport, otelhttp.WithTracerProvider(clientTP))}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	root.End()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("checkout status %d", resp.StatusCode)
	}
	for _, tp := range []*sdktrace.TracerProvider{clientTP, coTP, payTP} {
		if err := tp.ForceFlush(t.Context()); err != nil {
			t.Fatalf("flush: %v", err)
		}
	}

	// Jaeger indexes asynchronously; poll until all three services have arrived.
	var got jaegerTrace
	services := map[string]bool{}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		got, services = fetch(t, jaeger, traceID)
		if services["e2e-client"] && services["checkout"] && services["payment"] {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !services["e2e-client"] || !services["checkout"] || !services["payment"] {
		t.Fatalf("trace %s in Jaeger has services %v, want e2e-client, checkout and payment", traceID, services)
	}

	tr := got.Data[0]
	byOp := map[string]string{}   // operation -> spanID
	parent := map[string]string{} // spanID -> parent spanID
	service := map[string]string{}
	for _, s := range tr.Spans {
		byOp[s.OperationName] = s.SpanID
		service[s.SpanID] = tr.Processes[s.ProcessID].ServiceName
		for _, r := range s.References {
			if r.RefType == "CHILD_OF" {
				parent[s.SpanID] = r.SpanID
			}
		}
	}
	for _, op := range []string{"place order", "POST /checkout", "validate order", "check stock", "SELECT inventory", "charge payment", "POST /charge"} {
		if byOp[op] == "" {
			t.Errorf("Jaeger trace has no %q span; operations: %v", op, keys(byOp))
		}
	}
	// payment's server span hangs off a checkout client span, which hangs off "charge payment".
	payServer := byOp["POST /charge"]
	if svc := service[parent[payServer]]; svc != "checkout" {
		t.Errorf("payment server span's parent belongs to %q, want checkout", svc)
	}
	if parent[parent[payServer]] != byOp["charge payment"] {
		t.Errorf("payment call is not under the charge payment span")
	}
	if parent[byOp["POST /checkout"]] == "" || service[parent[byOp["POST /checkout"]]] != "e2e-client" {
		t.Errorf("checkout server span is not a child of the test's client span")
	}
}

func fetch(t *testing.T, base, traceID string) (jaegerTrace, map[string]bool) {
	t.Helper()
	var out jaegerTrace
	resp, err := http.Get(fmt.Sprintf("%s/api/traces/%s", base, traceID))
	if err != nil {
		return out, nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil || len(out.Data) == 0 {
		return out, nil
	}
	services := map[string]bool{}
	for _, s := range out.Data[0].Spans {
		services[out.Data[0].Processes[s.ProcessID].ServiceName] = true
	}
	return out, services
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
