package work

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// DequeueFunc generates a job.
type DequeueFunc func(*DequeueOptions) (*Job, error)

// DequeueMiddleware modifies DequeueFunc behavior.
type DequeueMiddleware func(DequeueFunc) DequeueFunc

// EnqueueFunc takes in a job for processing.
type EnqueueFunc func(*Job, *EnqueueOptions) error

// EnqueueMiddleware modifies EnqueueFunc behavior.
type EnqueueMiddleware func(EnqueueFunc) EnqueueFunc

// HandleFunc runs a job.
type HandleFunc func(*Job, *DequeueOptions) error

// HandleMiddleware modifies HandleFunc hehavior.
type HandleMiddleware func(HandleFunc) HandleFunc

type handler struct {
	QueueID    string
	HandleFunc HandleFunc
	JobOptions JobOptions
}

// WorkerOptions is used to create a worker.
type WorkerOptions struct {
	Namespace string
	Queue     Queue
}

// Worker runs jobs.
type Worker struct {
	namespace string
	queue     Queue

	stop       chan struct{}
	wg         sync.WaitGroup
	handlerMap map[string]handler
}

// NewWorker creates a new worker.
func NewWorker(opt *WorkerOptions) *Worker {
	return &Worker{
		namespace:  opt.Namespace,
		queue:      opt.Queue,
		handlerMap: make(map[string]handler),
	}
}

// JobOptions specifies how a job is executed.
type JobOptions struct {
	MaxExecutionTime time.Duration
	IdleWait         time.Duration
	NumGoroutines    int64

	DequeueMiddleware []DequeueMiddleware
	HandleMiddleware  []HandleMiddleware
}

// AddDequeueMiddleware adds DequeueMiddleware.
func (opt *JobOptions) AddDequeueMiddleware(mw DequeueMiddleware) *JobOptions {
	opt.DequeueMiddleware = append(opt.DequeueMiddleware, mw)
	return opt
}

// AddHandleMiddleware adds HandleMiddleware.
func (opt *JobOptions) AddHandleMiddleware(mw HandleMiddleware) *JobOptions {
	opt.HandleMiddleware = append(opt.HandleMiddleware, mw)
	return opt
}

// options validation error
var (
	ErrMaxExecutionTime = errors.New("work: max execution time should be > 0")
	ErrNumGoroutines    = errors.New("work: number of goroutines should be > 0")
	ErrIdleWait         = errors.New("work: idle wait should be > 0")
)

// Validate validates JobOptions.
func (opt *JobOptions) Validate() error {
	if opt.MaxExecutionTime <= 0 {
		return ErrMaxExecutionTime
	}
	if opt.IdleWait <= 0 {
		return ErrIdleWait
	}
	if opt.NumGoroutines <= 0 {
		return ErrNumGoroutines
	}
	return nil
}

var (
	// ErrQueueNotFound is returned if the queue is not yet
	// defined with Register().
	ErrQueueNotFound = errors.New("work: queue is not found")

	// ErrUnrecoverable is returned if the error is unrecoverable.
	// The job will be discarded.
	ErrUnrecoverable = errors.New("work: permanent error")

	// ErrUnsupported is returned if it is not implemented.
	ErrUnsupported = errors.New("work: unsupported")
)

// Register adds handler for a queue.
func (w *Worker) Register(queueID string, h HandleFunc, opt *JobOptions) error {
	err := opt.Validate()
	if err != nil {
		return err
	}
	w.handlerMap[queueID] = handler{
		QueueID:    queueID,
		HandleFunc: h,
		JobOptions: *opt,
	}
	return nil
}

// Start starts the worker.
func (w *Worker) Start() {
	w.stop = make(chan struct{})
	for _, h := range w.handlerMap {
		for i := int64(0); i < h.JobOptions.NumGoroutines; i++ {
			w.wg.Add(1)
			go w.start(h)
		}
	}
}

func (w *Worker) start(h handler) {
	defer w.wg.Done()

	dequeue := getDequeueFunc(w.queue)
	for _, mw := range h.JobOptions.DequeueMiddleware {
		dequeue = mw(dequeue)
	}
	dequeue = idleWait(h.JobOptions.IdleWait, w.stop)(dequeue)

	handle := h.HandleFunc
	for _, mw := range h.JobOptions.HandleMiddleware {
		handle = mw(handle)
	}
	handle = catchPanic(handle)
	handle = retry(w.queue)(handle)

	// prepare bulk ack flush
	var ackJobs []*Job
	flush := func() error {
		opt := &AckOptions{
			Namespace: w.namespace,
			QueueID:   h.QueueID,
		}
		bulkDeq, ok := w.queue.(bulkDequeuer)
		if ok {
			err := bulkDeq.BulkAck(ackJobs, opt)
			if err != nil {
				return err
			}
			ackJobs = nil
			return nil
		}
		for _, job := range ackJobs {
			err := w.queue.Ack(job, opt)
			if err != nil {
				return err
			}
		}
		ackJobs = nil
		return nil
	}
	defer flush()

	const flushIntv = time.Second
	flushTicker := time.NewTicker(flushIntv)
	defer flushTicker.Stop()

	for {
		select {
		case <-w.stop:
			return
		case <-flushTicker.C:
			flush()
		default:
			func() error {
				opt := &DequeueOptions{
					Namespace:    w.namespace,
					QueueID:      h.QueueID,
					At:           time.Now(),
					InvisibleSec: int64(2 * (h.JobOptions.MaxExecutionTime + flushIntv) / time.Second),
				}
				job, err := dequeue(opt)
				if err != nil {
					return err
				}
				err = handle(job, opt)
				if err != nil {
					return err
				}
				ackJobs = append(ackJobs, job)
				if len(ackJobs) >= 1000 {
					// prevent un-acked job count to be too large
					flush()
				}
				return nil
			}()
		}
	}
}

func getDequeueFunc(queue Queue) DequeueFunc {
	bulkDeq, ok := queue.(bulkDequeuer)
	if !ok {
		return queue.Dequeue
	}

	var jobs []*Job
	return func(opt *DequeueOptions) (*Job, error) {
		if len(jobs) == 0 {
			const count = 1000
			bulkOpt := *opt
			bulkOpt.InvisibleSec *= count

			var err error
			jobs, err = bulkDeq.BulkDequeue(count, &bulkOpt)
			if err != nil {
				return nil, err
			}
		}
		job := jobs[0]
		jobs = jobs[1:]
		return job, nil
	}
}

// ExportMetrics dumps queue stats if the queue implements MetricsExporter.
func (w *Worker) ExportMetrics() (*Metrics, error) {
	exporter, ok := w.queue.(MetricsExporter)
	if !ok {
		return nil, ErrUnsupported
	}
	var (
		queueMetrics []*QueueMetrics
	)
	for _, h := range w.handlerMap {
		m, err := exporter.GetQueueMetrics(&QueueMetricsOptions{
			Namespace: w.namespace,
			QueueID:   h.QueueID,
			At:        time.Now(),
		})
		if err != nil {
			return nil, err
		}
		queueMetrics = append(queueMetrics, m)
	}
	return &Metrics{
		Queue: queueMetrics,
	}, nil
}

// Stop stops the worker.
func (w *Worker) Stop() {
	close(w.stop)
	w.wg.Wait()
}

func idleWait(d time.Duration, stop <-chan struct{}) DequeueMiddleware {
	return func(f DequeueFunc) DequeueFunc {
		return func(opt *DequeueOptions) (*Job, error) {
			job, err := f(opt)
			if err != nil {
				if err == ErrEmptyQueue {
					select {
					case <-time.After(d):
					case <-stop:
					}
				}
				return nil, err
			}
			return job, nil
		}
	}
}

func catchPanic(f HandleFunc) HandleFunc {
	return func(job *Job, opt *DequeueOptions) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = errors.New(fmt.Sprint(r))
			}
		}()
		return f(job, opt)
	}
}

func retry(queue Queue) HandleMiddleware {
	return func(f HandleFunc) HandleFunc {
		return func(job *Job, opt *DequeueOptions) error {
			err := f(job, opt)
			if err != nil && err != ErrUnrecoverable {
				now := time.Now()
				job.Retries++
				job.LastError = err.Error()
				job.UpdatedAt = now
				job.EnqueuedAt = now.Add(time.Duration(job.Retries*opt.InvisibleSec) * time.Second)
				queue.Enqueue(job, &EnqueueOptions{
					Namespace: opt.Namespace,
					QueueID:   opt.QueueID,
				})
				return err
			}
			return nil
		}
	}
}
