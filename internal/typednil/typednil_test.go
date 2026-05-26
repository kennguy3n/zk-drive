package typednil_test

import (
	"testing"

	"github.com/kennguy3n/zk-drive/internal/typednil"
)

// concretePtr models any With* setter input that is backed by a
// pointer receiver — the canonical typed-nil hazard. The setter at
// the call site stores the interface; if a typed-nil pointer slips
// past the `x != nil` guard, the first method dispatch panics.
type concretePtr struct{}

func (*concretePtr) Method() {}

type iface interface {
	Method()
}

func TestIsTypedNil(t *testing.T) {
	t.Parallel()
	var (
		nilPtr     *concretePtr
		nilSlice   []string
		nilMap     map[string]string
		nilChan    chan int
		nilFunc    func()
		nilIfaceTN iface = nilPtr // typed-nil interface — the bug we exist to catch
	)
	cases := []struct {
		name string
		in   any
		want bool
	}{
		{"untyped nil", nil, true},
		{"typed-nil concrete pointer", nilPtr, true},
		{"typed-nil interface wrapping nil pointer", nilIfaceTN, true},
		{"nil slice", nilSlice, true},
		{"nil map", nilMap, true},
		{"nil chan", nilChan, true},
		{"nil func", nilFunc, true},
		{"non-nil concrete pointer", &concretePtr{}, false},
		{"non-nil string", "hello", false},
		{"non-nil int", 42, false},
		{"non-nil struct", struct{}{}, false},
		{"non-nil slice", []string{"a"}, false},
		{"empty (non-nil) slice", []string{}, false},
		{"non-nil map", map[string]string{"k": "v"}, false},
		{"empty (non-nil) map", map[string]string{}, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := typednil.IsTypedNil(tc.in); got != tc.want {
				t.Errorf("IsTypedNil(%v) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}
}
