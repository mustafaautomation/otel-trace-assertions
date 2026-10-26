# otel-trace-assertions

[![CI](https://github.com/mustafaautomation/otel-trace-assertions/actions/workflows/ci.yml/badge.svg)](https://github.com/mustafaautomation/otel-trace-assertions/actions/workflows/ci.yml)

Trace-based testing in Go: tests that assert on the OpenTelemetry spans a service produces, not
just on its HTTP responses. Traces show things a response cannot: N+1 queries, a call that skipped
the payment service, context that did not propagate across a service boundary, a card number
leaking into telemetry, or errors flagged on the wrong span.

Personal portfolio project, not production or client work.

## What is here

- **`traceassert`**: a small fluent assertion package over spans recorded by the OTel SDK's
  `tracetest.SpanRecorder`. When an assertion fails, it prints the whole trace as a tree:

  ```
  spans name="SELECT inventory": want at most 1, got 5

  trace:
  - POST /checkout [checkout, server] 6.317ms
    - validate order [checkout, internal] 56µs
    - check stock [checkout, internal] 5.699ms
      - SELECT inventory [checkout, client] 1.147ms
      - SELECT inventory [checkout, client] 1.132ms
      ...
    - charge payment [checkout, internal] 530µs
      - HTTP POST [checkout, client] 486µs
        - POST /charge [payment, server] 31µs
  ```

  Selectors: `Named`, `Service`, `Where`. Assertions: `Count`, `AtMost`, `One`, `Attr`, `HasAttr`,
  `NoAttr`, `Status`, `Kind`, `Event`, `ChildOf`, `DescendantOf`, `SameTrace`, `Within`.
- **Two instrumented services**: `checkout` (validates, checks stock in a "database", charges a card)
  and `payment`. Both use `otelhttp` with W3C trace-context propagation, and the database spans
  follow the OTel database semantic conventions.
- **`cmd/checkoutd`**: runs both services with an OTLP/HTTP exporter, for looking at real traces in
  Jaeger.

## What the tests assert

| Test | What the trace must show |
|---|---|
| Happy path shape | A server span `POST /checkout` with HTTP semconv attributes (`http.request.method`, `http.response.status_code`, `http.route`), children `validate order`, `check stock` (one batched DB query), `charge payment`, then an HTTP client span, then the **payment service's** server span, all in one trace |
| No card number in telemetry | No attribute anywhere contains the full card number; only `payment.card.last4` |
| Declined payment | `charge payment` has status Error and an `exception` event. The server spans stay **Unset** for the 402, because the semantic conventions treat 4xx as the client's problem |
| Payment outage | The checkout server span is Error (502), and the payment service has no spans |
| Out of stock | Error and an `out_of_stock` event on `check stock`; no payment span at all |
| Invalid order | Validation fails before any database call |
| Query count does not grow with order size | 1, 3 and 5 items each make at most one inventory query |
| N+1 is caught | With batching switched off, the same `AtMost(1)` assertion fails with "got 5" and prints the tree |

`traceassert` itself has a passing and a failing test for every assertion, and checks that each
failure message carries the trace tree.

The first version of the happy-path test failed because `http.route` was missing. `otelhttp` wraps
the whole `ServeMux`, so it cannot see which route matched. The services now set `http.route`
themselves, which is the kind of gap these tests are for.

## End-to-end against Jaeger

`e2e/jaeger_test.go` (build tag `e2e`) exports real spans over OTLP/HTTP to Jaeger 2.22, then reads
the stored trace back from Jaeger's query API. It checks that:

- all three services (the test client, checkout and payment) are in one trace;
- every expected operation was stored;
- the payment server span's parent is checkout's HTTP client span, under `charge payment`.

This tests the exporter, propagation and backend path, not just in-memory spans. CI runs Jaeger as
a service container.

```
docker run -d -p 16686:16686 -p 4318:4318 jaegertracing/jaeger:2.22.0
go test -tags e2e ./e2e/
go run ./cmd/checkoutd   # then POST to localhost:8090/checkout and open localhost:16686
```

## CI

| Job | What it runs |
|---|---|
| golangci-lint | golangci-lint 2.15 with gosec, revive, errorlint, bodyclose and contextcheck (including the e2e file) |
| Trace assertions | `go vet`, all tests with `-race` and coverage (96% of the service and assertion packages locally), then the trace tests 10 more times to catch flakiness |
| e2e | The Jaeger round trip above |

Stack: Go 1.27, OpenTelemetry Go SDK 1.47, otelhttp 0.72, Jaeger 2.22.

## Limits

- The in-memory tests use one process. The "services" are separate tracer providers with their own
  `service.name`, talking over real HTTP on localhost.
- The database is simulated: spans are emitted as a SQL client would, with no real database.
- This is a hand-rolled assertion helper, not a product like Tracetest. It is meant to show the
  technique in plain `go test`.

## License

MIT
