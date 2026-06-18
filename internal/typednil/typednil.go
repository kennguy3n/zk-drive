// Package typednil detects "typed nil" interface values — interface
// slots where the type word is non-nil but the value word is nil.
// A plain `v == nil` comparison on a typed-nil interface returns
// false (Go's interface equality compares BOTH words), which is the
// source of many subtle NPE bugs at first method-dispatch on a
// "looks-nil-but-isn't" interface.
//
// The canonical use case is the With* setter pattern: a constructor
// or boot-time wiring helper that takes an interface and stores it
// on a struct. If a caller passes (*ConcreteType)(nil) wrapped in
// the interface, the setter's `if x != nil { h.x = x }` guard does
// NOT fire, the typed-nil is stored, and the first call into
// h.x.Method() panics inside the request handler at runtime.
// IsTypedNil at the setter boundary normalises both the untyped
// nil and the typed-nil cases to a plain nil field.
//
// The helper used to live as an unexported `isTypedNil` inside
// api/drive, but the AI services in internal/ai have the same
// setter pattern (six With* setters across SummaryService,
// SuggestionService, and ExpansionService) and api/drive imports
// internal/ai, not the other way around. Extracting to this
// neutral package lets both import sites share a single source of
// truth for what "typed nil" means — same Kind switch, same edge
// cases, same future extensions.
package typednil

import "reflect"

// IsTypedNil reports whether v is an interface value whose
// concrete kind is nilable (pointer, map, chan, func, slice) AND
// whose underlying value is nil. Returns true for the plain
// untyped-nil case too — callers that want to distinguish should
// `v == nil` first, but in practice every existing caller (the
// With* setter pattern) wants both normalised together. See the
// package doc for the rationale.
//
// Why a switch on reflect.Kind and not a single rv.IsNil() call:
// IsNil() panics for non-nilable kinds (struct, int, string, …),
// so the switch acts as a guard AND the documentation of which
// kinds count. The reflect.Interface case used to appear in the
// switch defensively but it was dead code: reflect.ValueOf on an
// `any` parameter unwraps one level of interface, so rv.Kind()
// can never be reflect.Interface here, so that arm is omitted.
func IsTypedNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Chan, reflect.Func, reflect.Slice:
		return rv.IsNil()
	}
	return false
}
