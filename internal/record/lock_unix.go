//go:build unix

package record

import "github.com/CoachCoe/Ledger/internal/lock"

func lockFile(path string) (func(), error) { return lock.Acquire(path, false) }
