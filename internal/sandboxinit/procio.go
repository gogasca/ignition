package sandboxinit

import "sync"

// Channel names on the wire, matching ExecChannel in process.proto.
const (
	ChanStdout = "stdout"
	ChanStderr = "stderr"
)

type ioChunk struct {
	Channel string
	Data    []byte
}

// procIO fans a process's stdout/stderr out to any number of live attachers and
// keeps a bounded replay buffer so an attacher that connects slightly late
// still sees recent output. It is not a durable spool — that is a data-plane
// feature the MVP gateway does not have.
type procIO struct {
	mu        sync.Mutex
	buf       []ioChunk
	base      int64 // absolute index of buf[0]
	maxChunks int
	closed    bool
	waiters   map[chan struct{}]struct{}
}

func newProcIO() *procIO {
	return &procIO{maxChunks: 2048, waiters: map[chan struct{}]struct{}{}}
}

func (p *procIO) writer(ch string) *chanWriter { return &chanWriter{io: p, ch: ch} }

type chanWriter struct {
	io *procIO
	ch string
}

func (w *chanWriter) Write(b []byte) (int, error) {
	cp := make([]byte, len(b))
	copy(cp, b)
	w.io.append(ioChunk{Channel: w.ch, Data: cp})
	return len(b), nil
}

func (p *procIO) append(c ioChunk) {
	p.mu.Lock()
	p.buf = append(p.buf, c)
	if len(p.buf) > p.maxChunks {
		drop := len(p.buf) - p.maxChunks
		p.buf = p.buf[drop:]
		p.base += int64(drop)
	}
	p.wakeLocked()
	p.mu.Unlock()
}

func (p *procIO) close() {
	p.mu.Lock()
	p.closed = true
	p.wakeLocked()
	p.mu.Unlock()
}

func (p *procIO) wakeLocked() {
	for w := range p.waiters {
		select {
		case w <- struct{}{}:
		default:
		}
	}
}

// stream calls fn for every chunk from absolute index `from` onward, blocking
// for new output, until the process closes and the backlog is drained (returns
// nil) or fn returns an error.
func (p *procIO) stream(from int64, fn func(ioChunk) error) error {
	notify := make(chan struct{}, 1)
	p.mu.Lock()
	p.waiters[notify] = struct{}{}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.waiters, notify)
		p.mu.Unlock()
	}()

	next := from
	for {
		p.mu.Lock()
		if next < p.base {
			next = p.base
		}
		var pending []ioChunk
		if off := int(next - p.base); off < len(p.buf) {
			pending = append(pending, p.buf[off:]...)
			next = p.base + int64(len(p.buf))
		}
		done := p.closed
		p.mu.Unlock()

		for _, c := range pending {
			if err := fn(c); err != nil {
				return err
			}
		}
		if done {
			return nil
		}
		<-notify
	}
}
