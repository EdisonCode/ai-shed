package backlog

import (
	"slices"
	"sync"
	"time"
)

// readTTL is how long a read of GitHub is shared. It is shorter than the
// supervisor's 30-second check: within one check, or one read of the fleet
// by shed status or shed observe, every worker and every source of a
// repository share one read of its pull requests and of each issue list,
// and the next check reads afresh. The account's GitHub calls are shared
// with every machine, so a read repeated within a check costs them all.
const readTTL = 25 * time.Second

// readCache keeps recent reads by key. A failed read is not kept.
type readCache[T any] struct {
	mu   sync.Mutex
	now  func() time.Time
	kept map[string]cached[T]
}

type cached[T any] struct {
	at  time.Time
	val T
}

// get returns the value read for key within readTTL, or reads it. Reads of
// different keys run at once; two reads of the same key at once may both
// run, and the later one is kept.
func (c *readCache[T]) get(key string, read func() (T, error)) (T, error) {
	c.mu.Lock()
	if c.now == nil {
		c.now = time.Now
	}
	if v, ok := c.kept[key]; ok && c.now().Sub(v.at) < readTTL {
		c.mu.Unlock()
		return v.val, nil
	}
	c.mu.Unlock()
	val, err := read()
	if err != nil {
		return val, err
	}
	c.mu.Lock()
	if c.kept == nil {
		c.kept = map[string]cached[T]{}
	}
	c.kept[key] = cached[T]{at: c.now(), val: val}
	c.mu.Unlock()
	return val, nil
}

var (
	openPRCache   readCache[map[int]PR]
	mergedPRCache readCache[map[int]bool]
	listCache     readCache[[]Issue]
)

// sharedList returns a copy of a shared issue list, so that a caller that
// sets a field of an issue does not set it for the others.
func sharedList(issues []Issue) []Issue {
	return slices.Clone(issues)
}
