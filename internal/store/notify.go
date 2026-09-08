package store

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Change identifies a resource that may have changed. Kind is the schema table
// name ("sandboxes" or "operations"); ID is the resource ID.
type Change struct {
	Kind string
	ID   string
}

// ChangeNotifier is an optional Store capability. Subscribe returns a channel
// that receives a Change whenever a sandbox or operation row is written, plus a
// cancel func that releases the subscription. Signals are best-effort: a
// subscriber must still read authoritative state, and a missed or coalesced
// notification only costs latency. The :watch SSE handlers use this to wake
// immediately instead of polling on a fixed interval.
type ChangeNotifier interface {
	Subscribe() (<-chan Change, func())
}

// subHub is the shared fan-out used by both the in-memory store and the
// Postgres LISTEN listener.
type subHub struct {
	mu   sync.Mutex
	subs map[chan Change]struct{}
}

func newSubHub() *subHub { return &subHub{subs: map[chan Change]struct{}{}} }

func (h *subHub) Subscribe() (<-chan Change, func()) {
	ch := make(chan Change, 16)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			close(ch)
			h.mu.Unlock()
		})
	}
}

func (h *subHub) publish(c Change) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		// Drop rather than block: a slow watcher still catches up on its poll
		// backstop, and one watcher must never stall a writer.
		select {
		case ch <- c:
		default:
		}
	}
}

// Subscribe implements ChangeNotifier for the in-memory store.
func (m *Memory) Subscribe() (<-chan Change, func()) { return m.hub.Subscribe() }

// notify is called from every sandbox/operation write path in the memory store.
func (m *Memory) notify(kind, id string) {
	if m.hub != nil {
		m.hub.publish(Change{Kind: kind, ID: id})
	}
}

// watchHub owns a dedicated LISTEN connection and reconnects on failure.
type watchHub struct {
	*subHub
	cancel context.CancelFunc
}

func newWatchHub(dsn string) *watchHub {
	ctx, cancel := context.WithCancel(context.Background())
	h := &watchHub{subHub: newSubHub(), cancel: cancel}
	go h.run(ctx, dsn)
	return h
}

func (h *watchHub) stop() { h.cancel() }

func (h *watchHub) run(ctx context.Context, dsn string) {
	for ctx.Err() == nil {
		if err := h.listen(ctx, dsn); err != nil && ctx.Err() == nil {
			log.Printf("store: watch LISTEN dropped: %v; reconnecting", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (h *watchHub) listen(ctx context.Context, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN ignition_watch"); err != nil {
		return err
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		if kind, id, ok := strings.Cut(n.Payload, ":"); ok && id != "" {
			h.publish(Change{Kind: kind, ID: id})
		}
	}
}

// Subscribe implements ChangeNotifier for Postgres. The dedicated LISTEN
// connection is started on first use and torn down by Close. After Close it
// returns an inert (already-closed) channel so callers degrade to polling
// rather than panic.
func (p *Postgres) Subscribe() (<-chan Change, func()) {
	p.hubMu.Lock()
	defer p.hubMu.Unlock()
	if p.closed {
		ch := make(chan Change)
		close(ch)
		return ch, func() {}
	}
	if p.hub == nil {
		p.hub = newWatchHub(p.dsn)
	}
	return p.hub.Subscribe()
}
