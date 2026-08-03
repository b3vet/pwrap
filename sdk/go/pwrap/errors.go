package pwrap

import "errors"

var (
	// ErrNotFound is returned when a Get/Update/Delete targets a missing document.
	ErrNotFound = errors.New("pwrap: not found")
)
