package domain

import "errors"

var (
	ErrAccountingUnavailable   = errors.New("accounting_unavailable")
	ErrNotFound                = errors.New("carpool: not found")
	ErrRetentionPreviewInvalid = errors.New("carpool: retention preview invalid")
	ErrConflict                = errors.New("carpool: conflict")
	ErrBusy                    = errors.New("carpool: database busy")
	ErrInvalid                 = errors.New("carpool: invalid value")
	ErrAlreadyBootstrapped     = errors.New("carpool: administrator already bootstrapped")
	ErrMigration               = errors.New("carpool: migration error")
	ErrAuthorizationRejected   = errors.New("carpool: authorization rejected")
)
