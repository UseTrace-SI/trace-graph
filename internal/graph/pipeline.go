package graph

import (
	"crypto/sha256"
	"fmt"
	"runtime"
)

// indexJob is one source of a generation. The walk fills the first group of
// fields; a worker fills the results of a changed source.
type indexJob struct {
	rel, path string
	src       []byte
	key       string
	cached    cachedFile
	known     bool
	// unchanged sources matched their stat key and were not read.
	unchanged   bool
	typeChecked bool

	done      chan struct{}
	digest    string
	identical bool // content matches the cached digest
	batch     *fileBatch
	count     int
	err       error
}

// analyze hashes the source and, when its content changed, parses it into a
// batch. It never touches the database.
func (j *indexJob) analyze(root string, w *indexWriter, digestPrefix string) {
	j.digest = fmt.Sprintf("%s%x", digestPrefix, sha256.Sum256(j.src))
	if j.known && j.cached.digest == j.digest {
		j.identical = true
		return
	}
	j.batch = w.batch(j.rel)
	j.count, j.err = analyzeFile(root, j.rel, j.path, j.src, j.batch)
}

// indexPipeline runs jobs on a bounded worker pool and hands them back in
// submission order. Jobs in flight are bounded by count and source bytes,
// so memory stays proportional to the pool rather than the repository.
type indexPipeline struct {
	workers chan struct{}
	queue   []*indexJob
	bytes   int
}

const (
	maxIndexWorkers  = 16
	maxQueuedSources = 64 << 20
)

// indexWorkers is the size of the worker pool; tests vary it.
var indexWorkers = func() int { return min(runtime.GOMAXPROCS(0), maxIndexWorkers) }

func newIndexPipeline() *indexPipeline {
	return &indexPipeline{workers: make(chan struct{}, max(indexWorkers(), 1))}
}

// start runs work for job on the pool, waiting for a free worker.
func (p *indexPipeline) start(job *indexJob, work func()) {
	job.done = make(chan struct{})
	p.workers <- struct{}{}
	go func() {
		defer func() { <-p.workers }()
		defer close(job.done)
		work()
	}()
}

// push queues job and applies every finished job at the head of the queue,
// waiting for the head while the queue is over its bounds.
func (p *indexPipeline) push(job *indexJob, apply func(*indexJob) error) error {
	p.queue = append(p.queue, job)
	p.bytes += len(job.src)
	for len(p.queue) > 0 {
		head := p.queue[0]
		if head.done != nil && len(p.queue) <= 4*cap(p.workers) && p.bytes <= maxQueuedSources {
			select {
			case <-head.done:
			default:
				return nil
			}
		} else if head.done != nil {
			<-head.done
		}
		p.queue[0] = nil
		p.queue = p.queue[1:]
		p.bytes -= len(head.src)
		if err := apply(head); err != nil {
			return err
		}
	}
	return nil
}

// drain applies every remaining job in order.
func (p *indexPipeline) drain(apply func(*indexJob) error) error {
	for _, job := range p.queue {
		if job.done != nil {
			<-job.done
		}
		if err := apply(job); err != nil {
			return err
		}
	}
	p.queue = nil
	return nil
}
