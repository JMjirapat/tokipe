// Package safe contains the recover boundary tokipe puts around
// caller-supplied code.
//
// Every optimization stage calls something the caller wrote: a preprocess
// Rule, a tool Executor, a Compressor, an Embedder, a VectorStore. The
// fail-open guarantee (spec §2.5.1) says a failure in any of them must not
// break the turn — and a panic is a failure. Without this boundary, one
// panicking tool integration takes down the whole request, which is precisely
// the outcome fail-open exists to prevent.
//
// This is deliberately NOT applied to caller-supplied pipeline.Stage
// implementations added via config.WithStage. Those are the caller's own code
// running in the caller's own pipeline; swallowing their panics would hide
// their bugs rather than tolerate a third party's.
package safe

import "fmt"

// PanicError wraps a recovered panic so callers can distinguish it from an
// ordinary error with errors.As, and so a fail-open path can still report
// what happened.
type PanicError struct {
	Value any
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("recovered panic: %v", e.Value)
}

// Do runs fn, converting a panic into a *PanicError.
func Do(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{Value: r}
		}
	}()
	return fn()
}

// Value runs fn, converting a panic into a *PanicError and the zero value.
func Value[T any](fn func() (T, error)) (v T, err error) {
	defer func() {
		if r := recover(); r != nil {
			var zero T
			v, err = zero, &PanicError{Value: r}
		}
	}()
	return fn()
}

// Bool runs fn, returning false if it panics. For predicates such as
// Rule.CanHandle and Compressor.CanHandle, where a panic means "cannot".
func Bool(fn func() bool) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
		}
	}()
	return fn()
}

// Name runs fn, falling back to a placeholder if it panics or returns "".
//
// Name() looks too trivial to fail, which is exactly why it was missed: a
// panicking Name on an otherwise working Rule or ModelClient took down a turn
// that had already succeeded. Identification is never worth an outage.
func Name(fn func() string, fallback string) (name string) {
	defer func() {
		if r := recover(); r != nil {
			name = fallback
		}
	}()
	if n := fn(); n != "" {
		return n
	}
	return fallback
}
