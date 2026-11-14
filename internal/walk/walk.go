// Package walk runs a bounded pool of workers over a work list that grows
// as it is processed, such as directories discovered during a tree walk.
//
// The list is a LIFO stack, so the walk proceeds roughly depth-first and the
// backlog stays small: a breadth-first queue over a home directory can hold
// hundreds of thousands of pending paths, a stack holds a few thousand.
package walk

import (
	"context"
	"runtime"
	"sync"
)

// Workers picks a default pool size. Directory walking is bound by
// metadata latency rather than CPU, so a few more workers than cores keeps
// an SSD busy; the cap avoids thrashing spinning disks.
func Workers(n int) int {
	if n > 0 {
		return n
	}
	n = runtime.NumCPU() * 2
	if n < 4 {
		n = 4
	}
	if n > 16 {
		n = 16
	}
	return n
}

// Run calls fn for every item in seeds and every item fn pushes, using the
// given number of workers, and returns when all work is done or ctx is
// cancelled. Pending items are dropped on cancellation.
func Run[T any](ctx context.Context, workers int, seeds []T, fn func(item T, push func(...T))) {
	q := &queue[T]{items: append([]T(nil), seeds...)}
	q.cond = sync.NewCond(&q.mu)

	var wg sync.WaitGroup
	for i := 0; i < Workers(workers); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				item, ok := q.pop(ctx)
				if !ok {
					return
				}
				fn(item, q.push)
				q.finish()
			}
		}()
	}
	// Wake idle workers when the context ends so they can drain and exit.
	stop := context.AfterFunc(ctx, func() {
		q.mu.Lock()
		q.cond.Broadcast()
		q.mu.Unlock()
	})
	wg.Wait()
	stop()
}

type queue[T any] struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []T
	active int
	done   bool
}

func (q *queue[T]) pop(ctx context.Context) (T, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		if ctx.Err() != nil {
			q.items = nil
		}
		if n := len(q.items); n > 0 {
			item := q.items[n-1]
			var zero T
			q.items[n-1] = zero
			q.items = q.items[:n-1]
			q.active++
			return item, true
		}
		if q.active == 0 || q.done {
			q.done = true
			q.cond.Broadcast()
			var zero T
			return zero, false
		}
		q.cond.Wait()
	}
}

func (q *queue[T]) push(items ...T) {
	if len(items) == 0 {
		return
	}
	q.mu.Lock()
	q.items = append(q.items, items...)
	if len(items) == 1 {
		q.cond.Signal()
	} else {
		q.cond.Broadcast()
	}
	q.mu.Unlock()
}

func (q *queue[T]) finish() {
	q.mu.Lock()
	q.active--
	if q.active == 0 && len(q.items) == 0 {
		q.done = true
		q.cond.Broadcast()
	}
	q.mu.Unlock()
}
