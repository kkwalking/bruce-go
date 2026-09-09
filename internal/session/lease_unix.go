//go:build darwin || linux

package session

import (
	"fmt"
	"os"
	"syscall"
)

// AcquireTask prevents two processes from executing the same session. The OS
// releases the advisory lock even when the owner is killed without cleanup.
func (s *Store) AcquireTask() (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.File+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("session is already running in another process: %w", err)
	}
	release := func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }
	// Refresh after acquiring the lock; the previous process may have advanced
	// the session since this runtime loaded it.
	if err := s.openLocked(s.File); err != nil {
		release()
		return nil, err
	}
	return release, nil
}
