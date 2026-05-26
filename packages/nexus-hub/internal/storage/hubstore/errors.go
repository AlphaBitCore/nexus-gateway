// Package hubstore provides shared sentinel errors used by all Nexus Hub
// store sub-packages (authstore, enrollstore, registrystore, etc.) and
// the central Store facade. Having a single allocation for each sentinel
// means errors.Is comparisons succeed across package boundaries.
package hubstore

import "errors"

// ErrNotFound is returned when a query matches zero rows.
var ErrNotFound = errors.New("not found")

// ErrAmbiguous is returned by lookups whose match key alone cannot
// uniquely identify a row — currently raised by
// FindActiveAssignmentByIPAndTime when 2+ active DeviceAssignment rows
// share the same ip_address (NAT-shared egress).
var ErrAmbiguous = errors.New("ambiguous match")
