// Package doublebuffer provides a lock-free double-buffering mechanism for collections.
// Writers queue up and get exclusive access to modify a copy of the current collection.
// When finished, the instances are swapped. Readers always access the current instance.
package doublebuffer

import (
	"sync/atomic"
)

// DoubleBuffer wraps a collection with double-buffering for lock-free reads.
// T should be a struct containing maps/slices that need thread-safe access.
type DoubleBuffer[T any] struct {
	// Two instances of the collection
	instances [2]*T
	
	// Current instance index (0 or 1) - accessed atomically
	currentIdx int32
	
	// Writer queue - semaphore to allow one writer at a time
	writerSem chan struct{}
	
	// Tracks active readers for debugging
	activeReaders int64
	
	// Tracks active writers for debugging
	activeWriters int64
	
	// Copy function to deep copy the collection
	copyFn func(*T) *T
}

// NewDoubleBuffer creates a new double-buffered collection.
// initialValue: initial data for both buffers
// copyFn: function to deep copy the collection type T
func NewDoubleBuffer[T any](initialValue *T, copyFn func(*T) *T) *DoubleBuffer[T] {
	db := &DoubleBuffer[T]{
		instances: [2]*T{initialValue, copyFn(initialValue)},
		currentIdx: 0,
		writerSem:  make(chan struct{}, 1), // Binary semaphore
		copyFn:     copyFn,
	}
	// Allow first writer
	db.writerSem <- struct{}{}
	return db
}

// Read returns a read-only accessor for the current collection.
// The returned function uses only atomic loads (lock-free); it does not block writers.
func (db *DoubleBuffer[T]) Read() func() *T {
	atomic.AddInt64(&db.activeReaders, 1)
	
	// Return function that accesses the current instance
	// Note: We read the index atomically each time to handle swaps correctly
	// Since writers work on copies and swap atomically, this is safe
	return func() *T {
		defer atomic.AddInt64(&db.activeReaders, -1)
		// Read index atomically to get current instance
		currentIdx := atomic.LoadInt32(&db.currentIdx)
		return db.instances[currentIdx]
	}
}

// WriteSwap installs newValue as the current instance without copying from the old one.
// Only safe for bulk rebuild paths where the caller (under its own lock, e.g. recalcMu)
// reconstructs the whole collection and no other writer is active.
func (db *DoubleBuffer[T]) WriteSwap(newValue *T) {
	atomic.AddInt64(&db.activeWriters, 1)
	<-db.writerSem

	defer func() {
		atomic.AddInt64(&db.activeWriters, -1)
		db.writerSem <- struct{}{} // Release semaphore for next writer
	}()

	// Replace the non-current instance and swap; the other buffer becomes a stale
	// copy that the next Write overwrites via its normal copy-from-current.
	writeIdx := 1 - atomic.LoadInt32(&db.currentIdx)
	db.instances[writeIdx] = newValue
	atomic.StoreInt32(&db.currentIdx, int32(writeIdx))
}

// Write queues a write operation and provides exclusive access to a copy for modification.
// The modifyFn receives a copy of the current collection to modify.
// After modification, the instances are swapped atomically.
func (db *DoubleBuffer[T]) Write(modifyFn func(*T)) {
	// Wait for write permission (queued via semaphore)
	atomic.AddInt64(&db.activeWriters, 1)
	<-db.writerSem
	
	defer func() {
		atomic.AddInt64(&db.activeWriters, -1)
		db.writerSem <- struct{}{} // Release semaphore for next writer
	}()
	
	// Get the non-current instance (the one we'll modify)
	currentIdx := atomic.LoadInt32(&db.currentIdx)
	writeIdx := 1 - currentIdx
	
	// Deep copy current collection to write buffer
	db.instances[writeIdx] = db.copyFn(db.instances[currentIdx])
	
	// Modify the copy
	modifyFn(db.instances[writeIdx])
	
	// Atomically swap: make write buffer the new current
	atomic.StoreInt32(&db.currentIdx, int32(writeIdx))
}
