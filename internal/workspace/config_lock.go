package workspace

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/promptctl/primitives/filelock"
)

// ErrConfigBusy marks a config.json read-decide-write that gave up waiting for
// another process's hold on the config lock. Contention is terminal for this
// attempt only: the command may be retried unchanged once the holder exits.
var ErrConfigBusy = errors.New("workspace config busy")

// A hold spans one small file read and at most one temp-file write and rename,
// and its holder acquires nothing else, so a holder still in front after five
// seconds is stalled (stopped, or on a hung filesystem) rather than busy, and
// waiting longer would only hide it.
const (
	configLockAttempts   = 500
	configLockRetryDelay = 10 * time.Millisecond
)

// configLockPath mints the config lock beside config.json, in the storage dir
// every store lock lives in. It is minted here rather than by the store package
// that mints the others because resolution runs before any store exists, and
// this package sits below the store. See the lock discipline in the store
// package doc.
func configLockPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), ".links-config.lock")
}

// withConfigLock runs fn holding config.json's lock exclusively, so a read, the
// decision made from it, and the write that records the decision are one step
// to every other lit process. Two first runs of `lit init` would otherwise both
// read "no config", both mint a workspace id, and the later rename would
// silently replace the id the earlier one had already reported.
// [LAW:single-enforcer] every config.json writer passes through here.
// [LAW:no-ambient-temporal-coupling] the ordering has an owner, the kernel
// flock, instead of depending on which rename happens to land last.
func withConfigLock(configPath string, fn func() error) error {
	lockPath := configLockPath(configPath)
	release, acquired, err := filelock.Acquire(context.Background(), lockPath, true, configLockAttempts, configLockRetryDelay)
	if err != nil {
		return fmt.Errorf("lock workspace config: %w", err)
	}
	if !acquired {
		return fmt.Errorf("%w: another lit process held %s for over %s; retry once it exits",
			ErrConfigBusy, lockPath, time.Duration(configLockAttempts)*configLockRetryDelay)
	}
	fnErr := fn()
	if relErr := release(); relErr != nil {
		return errors.Join(fnErr, fmt.Errorf("release workspace config lock: %w", relErr))
	}
	return fnErr
}
