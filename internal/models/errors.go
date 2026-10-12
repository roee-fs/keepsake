package models

import "errors"

// ErrNotFound reports that a path has no concept in the tenant's scope.
var ErrNotFound = errors.New("not found")

// NotFoundError is ErrNotFound for one path.
type NotFoundError struct{ Path string }

func (e *NotFoundError) Error() string        { return ErrNotFound.Error() + ": " + e.Path }
func (e *NotFoundError) Is(target error) bool { return target == ErrNotFound }

// ErrUnavailable wraps every error that means the database is down.
var ErrUnavailable = errors.New("database unavailable")

// GrepError reports a pattern Postgres could not compile or match within the timeout.
type GrepError struct{ Msg string }

func (e *GrepError) Error() string { return e.Msg }
