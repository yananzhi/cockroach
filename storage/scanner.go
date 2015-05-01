// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Spencer Kimball (spencer.kimball@gmail.com)

package storage

import (
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/cockroachdb/cockroach/proto"
	"github.com/cockroachdb/cockroach/util"
	"github.com/cockroachdb/cockroach/util/hlc"
	"github.com/cockroachdb/cockroach/util/log"
)

// A rangeQueue is a prioritized queue of ranges for which work is
// scheduled. For example, there's a GC queue for ranges which are due
// for garbage collection, a rebalance queue to move ranges from full
// or busy stores, a recovery queue for ranges with dead replicas,
// etc.
type rangeQueue interface {
	// Start launches a goroutine to process the contents of the queue.
	// The provided stopper is used to signal that the goroutine should exit.
	Start(*hlc.Clock, *util.Stopper)
	// MaybeAdd adds the range to the queue if the range meets
	// the queue's inclusion criteria and the queue is not already
	// too full, etc.
	MaybeAdd(*Range, proto.Timestamp)
	// MaybeRemove removes the range from the queue if it is present.
	MaybeRemove(*Range)
}

// A rangeIterator provides access to a sequence of ranges to consider
// for inclusion in range queues. There are no requirements for the
// ordering of the iteration.
type rangeIterator interface {
	// Next returns the next range in the iteration. Returns nil if
	// there are no more ranges.
	Next() *Range
	// EstimatedCount returns the number of ranges estimated to remain
	// in the iteration. This value does not need to be exact.
	EstimatedCount() int
	// Reset restarts the iterator at the beginning.
	Reset()
}

// A storeStats holds statistics over the entire store. Stats is an
// aggregation of MVCC stats across all ranges in the store.
type storeStats struct {
	RangeCount int
	MVCC       proto.MVCCStats
}

// A rangeScanner iterates over ranges at a measured pace in order to
// complete approximately one full scan per interval. Each range is
// tested for inclusion in a sequence of prioritized range queues.
type rangeScanner struct {
	interval time.Duration  // Duration interval for scan loop
	iter     rangeIterator  // Iterator to implement scan of ranges
	queues   []rangeQueue   // Range queues managed by this scanner
	removed  chan *Range    // Ranges to remove from queues
	stats    unsafe.Pointer // Latest store stats object; updated atomically
	scanFn   func()         // Function called at each complete scan iteration
	// Count of times through the scanning loop but locked by the completedScan
	// mutex.
	completedScan *sync.Cond
	count         int64
}

// newRangeScanner creates a new range scanner with the provided loop interval,
// range iterator, and range queues.  If scanFn is not nil, after a complete
// loop that function will be called.
func newRangeScanner(interval time.Duration, iter rangeIterator, scanFn func()) *rangeScanner {
	return &rangeScanner{
		interval:      interval,
		iter:          iter,
		removed:       make(chan *Range, 10),
		stats:         unsafe.Pointer(&storeStats{RangeCount: iter.EstimatedCount()}),
		scanFn:        scanFn,
		completedScan: sync.NewCond(&sync.Mutex{}),
	}
}

// AddQueues adds a variable arg list of queues to the range scanner.
// This method may only be called before Start().
func (rs *rangeScanner) AddQueues(queues ...rangeQueue) {
	rs.queues = append(rs.queues, queues...)
}

// Start spins up the scanning loop. Call Stop() to exit the loop.
func (rs *rangeScanner) Start(clock *hlc.Clock, stopper *util.Stopper) {
	for _, queue := range rs.queues {
		queue.Start(clock, stopper)
	}
	rs.scanLoop(clock, stopper)
}

// Stats returns store stats from the most recently completed scan of
// all ranges. A scanner which hasn't fully scanned the ranges will
// return a stats object with MVCC stats empty and only an estimate
// for RangeCount.
func (rs *rangeScanner) Stats() storeStats {
	return *(*storeStats)(atomic.LoadPointer(&rs.stats))
}

// Count returns the number of times the scanner has cycled through
// all ranges.
func (rs *rangeScanner) Count() int64 {
	rs.completedScan.L.Lock()
	defer rs.completedScan.L.Unlock()
	return rs.count
}

// RemoveRange removes a range from any range queues the scanner may
// have placed it in. This method should be called by the Store
// when a range is removed (e.g. rebalanced or merged).
func (rs *rangeScanner) RemoveRange(rng *Range) {
	rs.removed <- rng
}

// WaitForScanCompletion waits until the end of the next scan and returns the
// total number of scans completed so far.
func (rs *rangeScanner) WaitForScanCompletion() int64 {
	rs.completedScan.L.Lock()
	defer rs.completedScan.L.Unlock()
	initalValue := rs.count
	for rs.count == initalValue {
		rs.completedScan.Wait()
	}
	return rs.count
}

// paceInterval returns a duration between iterations to allow us to pace
// the scan.
func (rs *rangeScanner) paceInterval(start, now time.Time) time.Duration {
	elapsed := now.Sub(start)
	remainingNanos := rs.interval.Nanoseconds() - elapsed.Nanoseconds()
	if remainingNanos < 0 {
		remainingNanos = 0
	}
	count := rs.iter.EstimatedCount()
	if count < 1 {
		count = 1
	}
	interval := time.Duration(remainingNanos / int64(count))
	return interval
}

// scanLoop loops endlessly, scanning through ranges available via
// the range iterator, or until the scanner is stopped. The iteration
// is paced to complete a full scan in approximately the scan interval.
func (rs *rangeScanner) scanLoop(clock *hlc.Clock, stopper *util.Stopper) {
	stopper.RunWorker(func() {
		start := time.Now()
		stats := &storeStats{}

		for {
			waitInterval := rs.paceInterval(start, time.Now())
			log.V(6).Infof("Wait time interval set to %s", waitInterval)
			select {
			case <-time.After(waitInterval):
				if !stopper.StartTask() {
					continue
				}
				rng := rs.iter.Next()
				if rng != nil {
					// Try adding range to all queues.
					for _, q := range rs.queues {
						q.MaybeAdd(rng, clock.Now())
					}
					stats.RangeCount++
					ms := rng.stats.GetMVCC()
					stats.MVCC.Accumulate(&ms)
				} else {
					// Otherwise, we're done with the iteration. Reset iteration and start time.
					rs.iter.Reset()
					start = time.Now()
					// Store the most recent scan results in the scanner's stats.
					atomic.StorePointer(&rs.stats, unsafe.Pointer(stats))
					stats = &storeStats{}
					if rs.scanFn != nil {
						rs.scanFn()
					}
					// Increment iteration count.
					rs.completedScan.L.Lock()
					rs.count++
					rs.completedScan.Broadcast()
					rs.completedScan.L.Unlock()
					log.V(6).Infof("reset range scan iteration")
				}
				stopper.FinishTask()

			case rng := <-rs.removed:
				// Remove range from all queues as applicable.
				for _, q := range rs.queues {
					q.MaybeRemove(rng)
				}
				log.V(6).Infof("removed range %s", rng)

			case <-stopper.ShouldStop():
				// Exit the loop.
				return
			}
		}
	})
}
