//go:build !darwin && !linux

package session

import "errors"

func (s *Store) AcquireTask() (func(), error) {
	return nil, errors.New("task checkpoint locking currently requires macOS or Linux")
}
