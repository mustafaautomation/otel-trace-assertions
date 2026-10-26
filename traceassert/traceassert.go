// Package traceassert makes assertions about OpenTelemetry spans captured in tests.
//
// It works on the spans an in-memory recorder collected (sdktrace.ReadOnlySpan). Failures print
// the whole trace as an indented tree, so a broken assertion shows what actually happened.
package traceassert

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// TB is the subset of testing.TB the package needs, so it can be tested with a fake.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
}

// Trace is every recorded span, usually from one or more SpanRecorders.
type Trace struct {
	t     TB
	spans []sdktrace.ReadOnlySpan
}

// New wraps recorded spans for assertions.
func New(t TB, spans ...[]sdktrace.ReadOnlySpan) *Trace {
	var all []sdktrace.ReadOnlySpan
	for _, s := range spans {
		all = append(all, s...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].StartTime().Before(all[j].StartTime()) })
	return &Trace{t: t, spans: all}
}

// Spans returns every span, ordered by start time.
func (tr *Trace) Spans() []sdktrace.ReadOnlySpan { return tr.spans }

// Set is a filtered selection of spans.
type Set struct {
	tr    *Trace
	desc  string
	spans []sdktrace.ReadOnlySpan
}

// Named selects spans with exactly this name.
func (tr *Trace) Named(name string) *Set {
	return tr.Where(fmt.Sprintf("name=%q", name), func(s sdktrace.ReadOnlySpan) bool { return s.Name() == name })
}

// Service selects spans whose resource has this service.name.
func (tr *Trace) Service(name string) *Set {
	return tr.Where(fmt.Sprintf("service=%q", name), func(s sdktrace.ReadOnlySpan) bool { return ServiceName(s) == name })
}

// Where selects spans matching a predicate; desc is used in failure messages.
func (tr *Trace) Where(desc string, keep func(sdktrace.ReadOnlySpan) bool) *Set {
	var out []sdktrace.ReadOnlySpan
	for _, s := range tr.spans {
		if keep(s) {
			out = append(out, s)
		}
	}
	return &Set{tr: tr, desc: desc, spans: out}
}

// Spans returns the selected spans.
func (s *Set) Spans() []sdktrace.ReadOnlySpan { return s.spans }

// Len is the number of selected spans.
func (s *Set) Len() int { return len(s.spans) }

func (s *Set) fail(format string, args ...any) {
	s.tr.t.Helper()
	s.tr.t.Errorf("spans %s: %s\n\ntrace:\n%s", s.desc, fmt.Sprintf(format, args...), s.tr.Tree())
}

// Count asserts the exact number of selected spans.
func (s *Set) Count(n int) *Set {
	s.tr.t.Helper()
	if len(s.spans) != n {
		s.fail("want %d, got %d", n, len(s.spans))
	}
	return s
}

// AtMost asserts an upper bound, e.g. to catch N+1 queries.
func (s *Set) AtMost(n int) *Set {
	s.tr.t.Helper()
	if len(s.spans) > n {
		s.fail("want at most %d, got %d", n, len(s.spans))
	}
	return s
}

// One asserts exactly one span is selected.
func (s *Set) One() *Set { s.tr.t.Helper(); return s.Count(1) }

// Attr asserts every selected span has the attribute with this value.
func (s *Set) Attr(key string, want any) *Set {
	s.tr.t.Helper()
	s.requireAny()
	for _, sp := range s.spans {
		got, ok := Attr(sp, key)
		if !ok {
			s.fail("span %q has no attribute %q", sp.Name(), key)
			continue
		}
		if fmt.Sprint(got.AsInterface()) != fmt.Sprint(want) {
			s.fail("span %q attribute %q = %v, want %v", sp.Name(), key, got.AsInterface(), want)
		}
	}
	return s
}

// HasAttr asserts every selected span has the attribute, whatever its value.
func (s *Set) HasAttr(keys ...string) *Set {
	s.tr.t.Helper()
	s.requireAny()
	for _, sp := range s.spans {
		for _, k := range keys {
			if _, ok := Attr(sp, k); !ok {
				s.fail("span %q has no attribute %q", sp.Name(), k)
			}
		}
	}
	return s
}

// NoAttr asserts no selected span has the attribute (e.g. no PII recorded).
func (s *Set) NoAttr(keys ...string) *Set {
	s.tr.t.Helper()
	for _, sp := range s.spans {
		for _, k := range keys {
			if v, ok := Attr(sp, k); ok {
				s.fail("span %q must not record %q (has %v)", sp.Name(), k, v.AsInterface())
			}
		}
	}
	return s
}

// Status asserts every selected span has this status code.
func (s *Set) Status(code codes.Code) *Set {
	s.tr.t.Helper()
	s.requireAny()
	for _, sp := range s.spans {
		if sp.Status().Code != code {
			s.fail("span %q status %v, want %v", sp.Name(), sp.Status().Code, code)
		}
	}
	return s
}

// Kind asserts every selected span has this span kind.
func (s *Set) Kind(kind trace.SpanKind) *Set {
	s.tr.t.Helper()
	s.requireAny()
	for _, sp := range s.spans {
		if sp.SpanKind() != kind {
			s.fail("span %q kind %v, want %v", sp.Name(), sp.SpanKind(), kind)
		}
	}
	return s
}

// Event asserts every selected span recorded an event with this name (e.g. "exception").
func (s *Set) Event(name string) *Set {
	s.tr.t.Helper()
	s.requireAny()
	for _, sp := range s.spans {
		found := false
		for _, e := range sp.Events() {
			if e.Name == name {
				found = true
			}
		}
		if !found {
			s.fail("span %q has no %q event", sp.Name(), name)
		}
	}
	return s
}

// ChildOf asserts every selected span's direct parent is in the other set.
func (s *Set) ChildOf(parent *Set) *Set {
	s.tr.t.Helper()
	s.requireAny()
	ids := map[trace.SpanID]bool{}
	for _, p := range parent.spans {
		ids[p.SpanContext().SpanID()] = true
	}
	for _, sp := range s.spans {
		if !ids[sp.Parent().SpanID()] {
			s.fail("span %q is not a child of %s", sp.Name(), parent.desc)
		}
	}
	return s
}

// DescendantOf asserts every selected span sits somewhere under a span of the other set.
func (s *Set) DescendantOf(ancestor *Set) *Set {
	s.tr.t.Helper()
	s.requireAny()
	byID := map[trace.SpanID]sdktrace.ReadOnlySpan{}
	for _, sp := range s.tr.spans {
		byID[sp.SpanContext().SpanID()] = sp
	}
	want := map[trace.SpanID]bool{}
	for _, a := range ancestor.spans {
		want[a.SpanContext().SpanID()] = true
	}
	for _, sp := range s.spans {
		found := false
		for cur := sp; cur != nil; {
			pid := cur.Parent().SpanID()
			if want[pid] {
				found = true
				break
			}
			cur = byID[pid]
		}
		if !found {
			s.fail("span %q is not under %s", sp.Name(), ancestor.desc)
		}
	}
	return s
}

// SameTrace asserts all selected spans share one trace ID (context propagated correctly).
func (s *Set) SameTrace() *Set {
	s.tr.t.Helper()
	s.requireAny()
	first := s.spans[0].SpanContext().TraceID()
	for _, sp := range s.spans[1:] {
		if sp.SpanContext().TraceID() != first {
			s.fail("span %q is in trace %s, others in %s", sp.Name(), sp.SpanContext().TraceID(), first)
		}
	}
	return s
}

// Within asserts every selected span finished within the duration.
func (s *Set) Within(d time.Duration) *Set {
	s.tr.t.Helper()
	s.requireAny()
	for _, sp := range s.spans {
		if took := sp.EndTime().Sub(sp.StartTime()); took > d {
			s.fail("span %q took %v, budget %v", sp.Name(), took, d)
		}
	}
	return s
}

func (s *Set) requireAny() {
	s.tr.t.Helper()
	if len(s.spans) == 0 {
		s.fail("no spans matched")
	}
}

// Attr returns a span attribute by key.
func Attr(s sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

// ServiceName returns the span's resource service.name.
func ServiceName(s sdktrace.ReadOnlySpan) string {
	if s.Resource() == nil {
		return ""
	}
	v, _ := s.Resource().Set().Value("service.name")
	return v.AsString()
}

// Tree renders every trace as an indented span tree with service, kind, status and duration.
func (tr *Trace) Tree() string {
	children := map[trace.SpanID][]sdktrace.ReadOnlySpan{}
	present := map[trace.SpanID]bool{}
	for _, s := range tr.spans {
		present[s.SpanContext().SpanID()] = true
	}
	var roots []sdktrace.ReadOnlySpan
	for _, s := range tr.spans {
		if p := s.Parent().SpanID(); p.IsValid() && present[p] {
			children[p] = append(children[p], s)
		} else {
			roots = append(roots, s)
		}
	}
	var b strings.Builder
	var walk func(s sdktrace.ReadOnlySpan, depth int)
	walk = func(s sdktrace.ReadOnlySpan, depth int) {
		status := ""
		if s.Status().Code == codes.Error {
			status = " ERROR"
			if s.Status().Description != "" {
				status += ": " + s.Status().Description
			}
		}
		fmt.Fprintf(&b, "%s- %s [%s, %s] %v%s\n", strings.Repeat("  ", depth), s.Name(), ServiceName(s),
			s.SpanKind(), s.EndTime().Sub(s.StartTime()).Round(time.Microsecond), status)
		for _, c := range children[s.SpanContext().SpanID()] {
			walk(c, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	return b.String()
}
