// Command checkoutd runs the checkout and payment services and exports traces over OTLP/HTTP,
// e.g. to a local Jaeger: docker run -p 16686:16686 -p 4318:4318 jaegertracing/jaeger:2.22.0
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/mustafaautomation/otel-trace-assertions/internal/checkout"
	"github.com/mustafaautomation/otel-trace-assertions/internal/payment"
	"github.com/mustafaautomation/otel-trace-assertions/internal/store"
	"github.com/mustafaautomation/otel-trace-assertions/internal/telemetry"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	otel.SetTextMapPropagator(propagation.TraceContext{})

	payTP, err := telemetry.OTLPProvider(ctx, "payment")
	if err != nil {
		log.Fatal(err)
	}
	coTP, err := telemetry.OTLPProvider(ctx, "checkout")
	if err != nil {
		log.Fatal(err)
	}
	inv := store.New(coTP, map[string]int{"SKU-1": 100, "SKU-2": 100, "SKU-3": 1}, 2*time.Millisecond)

	pay := &http.Server{Addr: "127.0.0.1:8091", Handler: payment.Handler(payTP, 50_000), ReadHeaderTimeout: 5 * time.Second}
	co := &http.Server{Addr: "127.0.0.1:8090", Handler: checkout.Handler(checkout.Config{
		Tracer: coTP, Inventory: inv, PaymentURL: "http://127.0.0.1:8091", BatchStockQuery: true,
	}), ReadHeaderTimeout: 5 * time.Second}
	for _, s := range []*http.Server{pay, co} {
		go func() {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatal(err)
			}
		}()
	}
	log.Println("checkout on :8090, payment on :8091")
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = co.Shutdown(shutdown)
	_ = pay.Shutdown(shutdown)
	_ = coTP.Shutdown(shutdown)
	_ = payTP.Shutdown(shutdown)
}
