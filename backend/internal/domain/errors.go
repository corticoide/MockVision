// Package domain holds MockVision's entities and business rules. It has no
// dependencies on infrastructure: storage, network and protocols live in
// other packages and depend on this one, never the other way around.
package domain

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNotFound is returned when an entity does not exist.
var ErrNotFound = errors.New("not found")

// FieldError describes a problem with one input field.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationError groups field problems found while validating an input.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		if f.Field == "" {
			parts = append(parts, f.Message)
			continue
		}
		parts = append(parts, f.Field+": "+f.Message)
	}
	return "invalid input: " + strings.Join(parts, "; ")
}

// Add records a field problem.
func (e *ValidationError) Add(field, format string, args ...any) {
	e.Fields = append(e.Fields, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
}

// Err returns the error when it has problems, or nil.
func (e *ValidationError) Err() error {
	if e == nil || len(e.Fields) == 0 {
		return nil
	}
	return e
}

// Invalid is a shortcut for a validation error on a single field.
func Invalid(field, format string, args ...any) error {
	v := &ValidationError{}
	v.Add(field, format, args...)
	return v
}

// ConflictError reports a violated uniqueness or state rule (RN-05, RN-03, RN-12...).
type ConflictError struct {
	Field   string
	Message string
}

func (e *ConflictError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return e.Field + ": " + e.Message
}

// Conflict builds a ConflictError.
func Conflict(field, format string, args ...any) error {
	return &ConflictError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// RejectedError reports an operation refused by a node policy, such as
// admission control (RN-16, D91). Code is stable and machine readable.
type RejectedError struct {
	Code   string
	Reason string
}

func (e *RejectedError) Error() string { return e.Reason }
