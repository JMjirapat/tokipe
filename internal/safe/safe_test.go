package safe_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/JMjirapat/tokipe/internal/safe"
)

// This package is the fail-open guarantee's foundation. It is exercised heavily
// through the extension matrix at the root, but that tests the *pipeline's*
// behaviour; these test the primitive directly, including the edges no stage
// happens to hit.

func TestDoReturnsFnError(t *testing.T) {
	sentinel := errors.New("boom")
	if err := safe.Do(func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the function's own error", err)
	}
	if err := safe.Do(func() error { return nil }); err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}

func TestDoConvertsPanicToPanicError(t *testing.T) {
	err := safe.Do(func() error { panic("exploded") })
	var pe *safe.PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v (%T), want *safe.PanicError", err, err)
	}
	if pe.Value != "exploded" {
		t.Errorf("Value = %v, want the panic value preserved", pe.Value)
	}
	if !strings.Contains(err.Error(), "exploded") {
		t.Errorf("Error() = %q, should mention the cause", err.Error())
	}
}

// A panic carrying an error value must keep that error reachable — losing it
// would turn a diagnosable failure into an opaque one.
func TestPanicWithAnErrorValueKeepsIt(t *testing.T) {
	inner := errors.New("the real cause")
	err := safe.Do(func() error { panic(inner) })

	var pe *safe.PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("want *safe.PanicError, got %T", err)
	}
	if got, ok := pe.Value.(error); !ok || !errors.Is(got, inner) {
		t.Errorf("Value = %v, want the original error", pe.Value)
	}
}

func TestValueReturnsResultAndContainsPanics(t *testing.T) {
	got, err := safe.Value(func() (int, error) { return 42, nil })
	if err != nil || got != 42 {
		t.Fatalf("Value = %d, %v; want 42, nil", got, err)
	}

	got, err = safe.Value(func() (int, error) { panic("nope") })
	if err == nil {
		t.Fatal("a panic must become an error")
	}
	if got != 0 {
		t.Errorf("value = %d, want the zero value after a panic", got)
	}
}

// The zero value must be returned for any type, including ones where "zero"
// is not obvious — a stale non-zero value after a panic would be worse than
// nothing, because the caller would act on it.
func TestValueReturnsTheZeroValueForCompositeTypes(t *testing.T) {
	type big struct {
		Name string
		Data []int
		M    map[string]int
	}
	got, err := safe.Value(func() (big, error) {
		panic("after partial construction")
	})
	if err == nil {
		t.Fatal("want an error")
	}
	if got.Name != "" || got.Data != nil || got.M != nil {
		t.Errorf("value = %+v, want the zero value", got)
	}

	ptr, err := safe.Value(func() (*big, error) { panic("x") })
	if err == nil || ptr != nil {
		t.Errorf("ptr = %v, err = %v; want nil, error", ptr, err)
	}
}

func TestValuePassesThroughAnOrdinaryError(t *testing.T) {
	sentinel := errors.New("ordinary")
	got, err := safe.Value(func() (string, error) { return "partial", sentinel })
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the function's error", err)
	}
	// An ordinary error is not a panic: whatever the function returned stands.
	if got != "partial" {
		t.Errorf("value = %q, want the function's own return", got)
	}
}

func TestBoolTreatsAPanicAsFalse(t *testing.T) {
	if !safe.Bool(func() bool { return true }) {
		t.Error("Bool should pass through true")
	}
	if safe.Bool(func() bool { return false }) {
		t.Error("Bool should pass through false")
	}
	// "Cannot answer" must read as "no": a predicate that panics has not
	// claimed anything, and treating it as true would run code on its word.
	if safe.Bool(func() bool { panic("cannot decide") }) {
		t.Error("a panicking predicate must be false, never true")
	}
}

func TestNameFallsBackOnPanicAndOnEmpty(t *testing.T) {
	cases := map[string]struct {
		fn   func() string
		want string
	}{
		"normal": {func() string { return "rag" }, "rag"},
		"panics": {func() string { panic("bad name") }, "fallback"},
		"empty":  {func() string { return "" }, "fallback"},
		"spaces": {func() string { return " " }, " "}, // only "" is treated as absent
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := safe.Name(tc.fn, "fallback"); got != tc.want {
				t.Errorf("Name = %q, want %q", got, tc.want)
			}
		})
	}
}

// nil panics are a real Go case (panic(nil) and, more often, a nil map write
// surfacing oddly). They must still be contained rather than read as success.
func TestNilPanicIsStillAnError(t *testing.T) {
	err := safe.Do(func() error {
		var m map[string]int
		m["boom"] = 1 // assignment to entry in nil map
		return nil
	})
	if err == nil {
		t.Fatal("a nil-map write must be contained as an error")
	}
	var pe *safe.PanicError
	if !errors.As(err, &pe) {
		t.Errorf("want *safe.PanicError, got %T", err)
	}
}

// Containment must not leak between calls: one panicking call must not affect
// the next.
func TestBoundariesAreIndependent(t *testing.T) {
	for range 100 {
		_ = safe.Do(func() error { panic("x") })
		if err := safe.Do(func() error { return nil }); err != nil {
			t.Fatalf("a later call was affected by an earlier panic: %v", err)
		}
	}
}
