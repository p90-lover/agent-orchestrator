package cdc

import "time"

// yieldRetry is how soon a janitor tries again after a pass gave the writer
// back to a chat write: sooner than its interval, or a busy store's backlog
// would never drain, but long enough for the queued writes to finish.
const yieldRetry = time.Minute

// minYieldingBatch is the smallest delete a janitor falls back to.
const minYieldingBatch int64 = 100

// yieldingBatch sizes a janitor's deletes to what the store can do without
// holding its single writer away from chat writes. A delete that had to yield
// (ports.ErrMaintenanceYielded) was too big for the disk's speed right now, so
// the next one is a quarter of it; each pass that finishes doubles it back
// toward the configured size. On a fast disk it never shrinks.
type yieldingBatch struct {
	full int64
	size int64
	// again is set by a pass that yielded: the janitor retries after yieldRetry.
	again bool
}

func newYieldingBatch(full int64) yieldingBatch {
	return yieldingBatch{full: full, size: full}
}

func (b *yieldingBatch) yielded() {
	b.again = true
	b.size = max(min(minYieldingBatch, b.full), b.size/4)
}

func (b *yieldingBatch) finished() {
	b.again = false
	b.size = min(b.full, b.size*2)
}

// wait is how long until the janitor's next pass.
func (b *yieldingBatch) wait(interval time.Duration) time.Duration {
	if b.again {
		return min(interval, yieldRetry)
	}
	return interval
}
