// Package unimpl marks the skeletons' function bodies (M1, M3).
//
// Every exported function of the skeleton that is not built yet returns (or, without an error result,
// panics with) an error wrapping ErrNotImplemented that names the operation and the work package that
// builds it. The package goes away when the last body is written.
package unimpl

import (
	"errors"
	"fmt"
)

// ErrNotImplemented is wrapped by every skeleton error.
var ErrNotImplemented = errors.New("not implemented")

// Error returns "<op>: not implemented (<wp>)", wrapping ErrNotImplemented.
func Error(op, wp string) error {
	return fmt.Errorf("%s: %w (%s)", op, ErrNotImplemented, wp)
}
