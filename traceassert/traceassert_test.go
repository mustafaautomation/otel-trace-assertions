package traceassert_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/mustafaautomation/otel-trace-assertions/traceassert"
)

type fakeTB struct{ errs []string }

func (f *fakeTB) Helper() {}
func (f *fakeTB) Errorf(format string, args ...any) {
	f.errs = append(f.errs, fmt.Sprintf(format, args...))
}

// sample records: root(server) -> [db(client) x2, work(internal, error, event)], plus a span in
// another trace and another service.
func sample(t *testing.T) ([]sdktrace.ReadOnlySpan, []sdktrace.ReadOnlySpan) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	res := resource.NewSchemaless(attribute.String("service.name", "api"))
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec), sdktrace.WithResource(res))
	tr := tp.Tracer("t")
	ctx, root := tr.Start(context.Background(), "GET /items", trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attribute.Int("http.response.status_code", 200), attribute.String("http.route", "/items")))
	for i := 0; i < 2; i++ {
		_, db := tr.Start(ctx, "SELECT items", trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(attribute.String("db.system.name", "postgresql")))
		db.End()
	}
	_, work := tr.Start(ctx, "work")
	work.RecordError(errors.New("boom"))
	work.SetStatus(codes.Error, "boom")
	time.Sleep(3 * time.Millisecond)
	work.End()
	root.End()

	otherRec := tracetest.NewSpanRecorder()
	otherTP := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(otherRec),
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", "worker"))))
	_, stray := otherTP.Tracer("t").Start(context.Background(), "job")
	stray.End()
	return rec.Ended(), otherRec.Ended()
}

func TestPassingAssertionsReportNothing(t *testing.T) {
	api, worker := sample(t)
	f := &fakeTB{}
	tr := traceassert.New(f, api, worker)
	root := tr.Named("GET /items").One().Kind(trace.SpanKindServer).Attr("http.response.status_code", 200).
		HasAttr("http.route").Status(codes.Unset)
	tr.Named("SELECT items").Count(2).AtMost(2).ChildOf(root).DescendantOf(root).Attr("db.system.name", "postgresql")
	tr.Named("work").One().Status(codes.Error).Event("exception").ChildOf(root).Within(time.Second)
	tr.Service("api").Count(4).SameTrace().NoAttr("user.email")
	tr.Service("worker").One()
	if len(f.errs) != 0 {
		t.Fatalf("unexpected failures: %v", f.errs)
	}
	if tr.Named("SELECT items").Len() != 2 || len(tr.Spans()) != 5 {
		t.Fatal("wrong selection sizes")
	}
}

func TestFailingAssertions(t *testing.T) {
	api, worker := sample(t)
	cases := []struct {
		name string
		run  func(tr *traceassert.Trace)
		want string
	}{
		{"count", func(tr *traceassert.Trace) { tr.Named("SELECT items").Count(1) }, "want 1, got 2"},
		{"at most", func(tr *traceassert.Trace) { tr.Named("SELECT items").AtMost(1) }, "want at most 1, got 2"},
		{"missing", func(tr *traceassert.Trace) { tr.Named("nope").Attr("a", 1) }, "no spans matched"},
		{"attr value", func(tr *traceassert.Trace) { tr.Named("GET /items").Attr("http.response.status_code", 500) },
			`attribute "http.response.status_code" = 200, want 500`},
		{"attr absent", func(tr *traceassert.Trace) { tr.Named("work").Attr("x", 1) }, `has no attribute "x"`},
		{"has attr", func(tr *traceassert.Trace) { tr.Named("work").HasAttr("x") }, `has no attribute "x"`},
		{"no attr", func(tr *traceassert.Trace) { tr.Named("GET /items").NoAttr("http.route") }, `must not record "http.route"`},
		{"status", func(tr *traceassert.Trace) { tr.Named("GET /items").Status(codes.Error) }, "status Unset, want Error"},
		{"kind", func(tr *traceassert.Trace) { tr.Named("work").Kind(trace.SpanKindClient) }, "kind internal, want client"},
		{"event", func(tr *traceassert.Trace) { tr.Named("GET /items").Event("exception") }, `no "exception" event`},
		{"child of", func(tr *traceassert.Trace) { tr.Named("GET /items").ChildOf(tr.Named("work")) }, "is not a child of"},
		{"descendant", func(tr *traceassert.Trace) { tr.Named("job").DescendantOf(tr.Named("GET /items")) }, "is not under"},
		{"same trace", func(tr *traceassert.Trace) {
			tr.Where("all", func(sdktrace.ReadOnlySpan) bool { return true }).SameTrace()
		}, "is in trace"},
		{"within", func(tr *traceassert.Trace) { tr.Named("work").Within(time.Microsecond) }, "budget 1µs"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeTB{}
			c.run(traceassert.New(f, api, worker))
			if len(f.errs) == 0 {
				t.Fatal("assertion did not fail")
			}
			if !strings.Contains(f.errs[0], c.want) {
				t.Fatalf("message %q does not contain %q", f.errs[0], c.want)
			}
			if !strings.Contains(f.errs[0], "trace:\n- ") {
				t.Fatalf("message should include the trace tree: %q", f.errs[0])
			}
		})
	}
}

func TestTreeShowsHierarchyAndErrors(t *testing.T) {
	api, worker := sample(t)
	tree := traceassert.New(&fakeTB{}, api, worker).Tree()
	for _, want := range []string{
		"- GET /items [api, server]",
		"  - SELECT items [api, client]",
		"  - work [api, internal]",
		"ERROR: boom",
		"- job [worker, internal]",
	} {
		if !strings.Contains(tree, want) {
			t.Errorf("tree missing %q:\n%s", want, tree)
		}
	}
}
