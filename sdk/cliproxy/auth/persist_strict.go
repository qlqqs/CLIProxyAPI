package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrStrictPersistenceConflict means runtime changed while a strict save was
// in progress. The caller must reload the account before retrying its mutation.
var ErrStrictPersistenceConflict = errors.New("strict persistence: account changed during save")

func (m *Manager) lockStrictPersistence(id string) func() {
	value, _ := m.strictPersistenceLocks.LoadOrStore(id, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

// reconcileStrictPersistence replaces a stale successful save with the latest
// runtime snapshot, without holding the global manager lock during storage I/O.
// A removed credential is deleted instead of resurrected. Ordinary lifecycle
// and refresh updates retain their existing best-effort semantics.
func (m *Manager) reconcileStrictPersistence(ctx context.Context, store Store, id string) error {
	for {
		m.mu.RLock()
		current := m.auths[id].Clone()
		epoch := m.authEpochs[id]
		m.mu.RUnlock()
		if current == nil {
			if errDelete := store.Delete(ctx, id); errDelete != nil {
				return fmt.Errorf("reconcile removed auth: %w", errDelete)
			}
		} else if _, errSave := store.Save(ctx, current); errSave != nil {
			return fmt.Errorf("reconcile changed auth: %w", errSave)
		}
		m.mu.RLock()
		latest := m.auths[id]
		stable := m.authEpochs[id] == epoch && ((current == nil && latest == nil) || (current != nil && latest != nil && current.Generation == latest.Generation && current.RegistrationEpoch == latest.RegistrationEpoch))
		m.mu.RUnlock()
		if stable {
			return nil
		}
		if errContext := ctx.Err(); errContext != nil {
			return errContext
		}
	}
}
