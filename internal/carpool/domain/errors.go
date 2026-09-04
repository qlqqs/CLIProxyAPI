package domain

import "errors"

var (
	ErrNotFound              = errors.New("carpool: not found")
	ErrConflict              = errors.New("carpool: conflict")
	ErrBusy                  = errors.New("carpool: database busy")
	ErrInvalid               = errors.New("carpool: invalid value")
	ErrAlreadyBootstrapped   = errors.New("carpool: administrator already bootstrapped")
	ErrMigration             = errors.New("carpool: migration error")
	ErrAuthorizationRejected = errors.New("carpool: authorization rejected")
)
