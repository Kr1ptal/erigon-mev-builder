package builder

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

var (
	maxThreads = max(1, int32((runtime.NumCPU()/3)*2)) // use 2/3 of available processors
)

func max(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

type Resubmitter struct {
	mu        sync.Mutex
	cancel    context.CancelFunc
	interrupt *int32
}

func (r *Resubmitter) newTask(repeatFor time.Duration, interval time.Duration, fn func() error) error {
	repeatUntilCh := time.After(repeatFor)

	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.mu.Unlock()

	firstRunErr := fn()

	go func() {
		for ctx.Err() == nil {
			select {
			case <-ctx.Done():
				return
			case <-repeatUntilCh:
				cancel()
				return
			case <-time.After(interval):
				fn()
			}
		}
	}()

	return firstRunErr
}

func (r *Resubmitter) newContinuousTask(repeatFor time.Duration, fn func() error) error {
	repeatUntilCh := time.After(repeatFor)

	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.mu.Unlock()

	firstRunErr := fn()

	go func() {
		if firstRunErr != nil {
			time.Sleep(500 * time.Millisecond)
		}

		for ctx.Err() == nil {
			select {
			case <-ctx.Done():
				return
			case <-repeatUntilCh:
				cancel()
				return
			default:
				err := fn()
				if err != nil {
					time.Sleep(500 * time.Millisecond)
				}
			}
		}
	}()

	return firstRunErr
}

func (r *Resubmitter) newMultiThreadTask(resubmitInterval time.Duration, fn func(*int32) error) error {
	r.mu.Lock()
	// cancel before interrupting, so we don't start building another block
	if r.cancel != nil {
		r.cancel()
	}
	if r.interrupt != nil {
		atomic.StoreInt32(r.interrupt, 1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	interrupt := new(int32)

	r.cancel = cancel
	r.interrupt = interrupt
	r.mu.Unlock()

	firstRunErr := fn(nil)

	go func() {
		taskCount := new(int32)

		for ctx.Err() == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(resubmitInterval):

				// submit up to maxThreads number of parallel builder tasks
				if atomic.LoadInt32(taskCount) < maxThreads {
					atomic.AddInt32(taskCount, 1)

					go func() {
						fn(interrupt)
						atomic.AddInt32(taskCount, -1)
					}()
				}
			}
		}
	}()

	return firstRunErr
}
