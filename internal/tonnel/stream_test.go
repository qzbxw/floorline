package tonnel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeMarketplace serves the two endpoints of the event API.
type fakeMarketplace struct {
	srv *httptest.Server

	mu      sync.Mutex
	replay  map[string][]string // after-cursor -> raw event JSON
	live    []string            // frames pushed the moment a socket connects
	dials   int
	queries []string
}

func newFakeMarketplace(t *testing.T) *fakeMarketplace {
	t.Helper()
	f := &fakeMarketplace{replay: map[string][]string{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/marketplace/events", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		after := r.URL.Query().Get("after")
		f.queries = append(f.queries, r.URL.RawQuery)
		raw := f.replay[after]
		f.mu.Unlock()

		if after == "expired" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"status":"error","message":"Invalid or expired after cursor"}`)
			return
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"status":"success","events":[%s],"nextAfter":null}`, strings.Join(raw, ","))
	})
	mux.HandleFunc("/api/marketplace/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()

		f.mu.Lock()
		f.dials++
		frames := append([]string{rawGreeting}, f.live...)
		f.mu.Unlock()

		ctx := r.Context()
		for _, frame := range frames {
			if err := c.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
				return
			}
		}
		<-ctx.Done()
	})

	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMarketplace) host() string { return strings.TrimPrefix(f.srv.URL, "https://") }

func event(id, kind, at string) string {
	return fmt.Sprintf(`{"eventId":%q,"version":1,"type":%q,"occurredAt":%q,"data":{"gift":{"gift_id":1,"gift_num":2,"gift_name":"Lol Pop","model":"Blood Sucker (1%%)","backdrop":"Black (1.5%%)","symbol":"Bat (0.5%%)"},"price":2.5,"asset":"TON","sale_type":"FIXED"}}`,
		id, kind, at)
}

// collector records what a stream processed, in order.
type collector struct {
	mu     sync.Mutex
	ids    []string
	cursor string
	got    chan struct{}
}

func newCollector() *collector { return &collector{got: make(chan struct{}, 64)} }

func (c *collector) handle(_ context.Context, ev Event) error {
	c.mu.Lock()
	c.ids = append(c.ids, ev.EventID)
	c.mu.Unlock()
	select {
	case c.got <- struct{}{}:
	default:
	}
	return nil
}

func (c *collector) save(_ context.Context, v string) error {
	c.mu.Lock()
	c.cursor = v
	c.mu.Unlock()
	return nil
}

func (c *collector) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ids...)
}

// waitFor blocks until n events have been processed, or the test times out.
func (c *collector) waitFor(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if len(c.seen()) >= n {
			return
		}
		select {
		case <-c.got:
		case <-deadline:
			t.Fatalf("only %d of %d events arrived: %v", len(c.seen()), n, c.seen())
		}
	}
}

// TestStreamReplaysBeforeLive is the contract's race-free recovery: frames that
// arrive while the gap is still being refilled must not be applied before the
// older events they may supersede.
func TestStreamReplaysBeforeLive(t *testing.T) {
	f := newFakeMarketplace(t)
	f.replay["seed"] = []string{
		event("r1", "listing.created", "2026-08-13T10:00:00.000Z"),
		event("r2", "listing.price_changed", "2026-08-13T10:00:01.000Z"),
	}
	// Deliberately out of order on the wire, and one of them is a replay event
	// the socket re-delivers — both of which really happen on reconnect.
	f.live = []string{
		event("l2", "listing.created", "2026-08-13T10:00:04.000Z"),
		event("r2", "listing.price_changed", "2026-08-13T10:00:01.000Z"),
		event("l1", "listing.created", "2026-08-13T10:00:03.000Z"),
	}

	c := newCollector()
	s := NewStream(StreamOptions{
		Host:       f.host(),
		HTTP:       f.srv.Client(),
		LoadCursor: func(context.Context) (string, error) { return "seed", nil },
		SaveCursor: c.save,
		Handle:     c.handle,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	c.waitFor(t, 4)
	got := c.seen()
	// The guarantee is that the gap is closed before anything live is applied,
	// and that the socket re-delivering a replayed event costs nothing. The
	// order of frames that arrive *after* the backlog is drained is the order
	// the server sent them in — the merge cannot reorder what it never held, so
	// asserting it here would be testing this test's own timing.
	if len(got) != 4 {
		t.Fatalf("processed %v, want four events with r2 deduplicated", got)
	}
	if got[0] != "r1" || got[1] != "r2" {
		t.Fatalf("processed %v, want the replayed gap first", got)
	}
	seen := map[string]int{}
	for _, id := range got {
		seen[id]++
	}
	for _, id := range []string{"r1", "r2", "l1", "l2"} {
		if seen[id] != 1 {
			t.Fatalf("%s processed %d times: %v", id, seen[id], got)
		}
	}

	// The cursor is only allowed to advance behind successful processing.
	deadline := time.After(10 * time.Second)
	for {
		c.mu.Lock()
		cur := c.cursor
		c.mu.Unlock()
		if cur == "l2" {
			break
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			t.Fatalf("cursor stuck at %q", cur)
		}
	}
	if !s.Healthy() {
		t.Fatal("stream reports unhealthy after a clean session")
	}
}

// TestStreamSkipsReplayWithoutACursor guards the cold start. Replaying from no
// cursor means paging seven days of the whole marketplace to arrive at a live
// feed we could have joined immediately.
func TestStreamSkipsReplayWithoutACursor(t *testing.T) {
	f := newFakeMarketplace(t)
	f.live = []string{event("l1", "listing.created", "2026-08-13T10:00:03.000Z")}

	c := newCollector()
	s := NewStream(StreamOptions{Host: f.host(), HTTP: f.srv.Client(), Handle: c.handle, SaveCursor: c.save})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	c.waitFor(t, 1)
	f.mu.Lock()
	queries := len(f.queries)
	f.mu.Unlock()
	if queries != 0 {
		t.Fatalf("replay was called %d times with no cursor to resume from", queries)
	}
}

// TestStreamRecoversFromAnExpiredCursor: retention is seven days, and a desk
// that was off for longer must rejoin live rather than stall on a 400.
func TestStreamRecoversFromAnExpiredCursor(t *testing.T) {
	f := newFakeMarketplace(t)
	f.live = []string{event("l1", "listing.created", "2026-08-13T10:00:03.000Z")}

	c := newCollector()
	s := NewStream(StreamOptions{
		Host:       f.host(),
		HTTP:       f.srv.Client(),
		LoadCursor: func(context.Context) (string, error) { return "expired", nil },
		SaveCursor: c.save,
		Handle:     c.handle,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	c.waitFor(t, 1)
	if got := c.seen(); got[0] != "l1" {
		t.Fatalf("processed %v after an expired cursor", got)
	}
}

// TestStreamRetriesAFailedHandler: a sale that failed to store must not be
// stepped over. The cursor stays put and the id is forgotten, so the next
// replay delivers it again.
func TestStreamRetriesAFailedHandler(t *testing.T) {
	f := newFakeMarketplace(t)
	f.replay["seed"] = []string{event("r1", "sale.completed", "2026-08-13T10:00:00.000Z")}

	var (
		mu     sync.Mutex
		calls  int
		saved  []string
		done   = make(chan struct{})
		closed bool
	)
	s := NewStream(StreamOptions{
		Host:       f.host(),
		HTTP:       f.srv.Client(),
		LoadCursor: func(context.Context) (string, error) { return "seed", nil },
		SaveCursor: func(_ context.Context, v string) error {
			mu.Lock()
			saved = append(saved, v)
			mu.Unlock()
			return nil
		},
		Handle: func(_ context.Context, ev Event) error {
			mu.Lock()
			calls++
			n := calls
			if n >= 2 && !closed {
				closed = true
				close(done)
			}
			mu.Unlock()
			if n == 1 {
				return fmt.Errorf("database is busy")
			}
			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the event was never retried after the handler failed")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, v := range saved {
		if v == "r1" && calls < 2 {
			t.Fatal("cursor advanced past an event the handler refused")
		}
	}
}

// The merge itself, without the timing: a backlog is applied oldest first,
// whatever order the socket delivered it in.
func TestBufferedEventsAreMergedInOccurredAtOrder(t *testing.T) {
	evs := []Event{}
	for _, raw := range []string{
		event("c", "listing.created", "2026-08-13T10:00:04.000Z"),
		event("a", "listing.created", "2026-08-13T10:00:01.000Z"),
		event("d", "listing.cancelled", "2026-08-13T10:00:04.000Z"), // ties keep arrival order
		event("b", "listing.price_changed", "2026-08-13T10:00:02.000Z"),
	} {
		ev, ok := DecodeEvent([]byte(raw))
		if !ok {
			t.Fatal("could not decode a test event")
		}
		evs = append(evs, ev)
	}
	sortByOccurredAt(evs)

	var ids []string
	for _, ev := range evs {
		ids = append(ids, ev.EventID)
	}
	if got := strings.Join(ids, ","); got != "a,b,c,d" {
		t.Fatalf("merged order %q, want oldest first with ties left as they arrived", got)
	}
}

func TestSeenSetEvictsInArrivalOrder(t *testing.T) {
	s := newSeenSet(2)
	if !s.add("a") || !s.add("b") {
		t.Fatal("fresh ids reported as duplicates")
	}
	if s.add("a") {
		t.Fatal("a repeated id was reported as new")
	}
	s.add("c") // evicts "a"
	if !s.add("a") {
		t.Fatal("the oldest id was not evicted once the window was full")
	}
	s.forget("b")
	if !s.add("b") {
		t.Fatal("a forgotten id was not deliverable again")
	}
}

func TestReconnectDelayBacksOffAndIsBounded(t *testing.T) {
	for attempt := 1; attempt <= 20; attempt++ {
		d := reconnectDelay(attempt)
		if d <= 0 || d > 90*time.Second {
			t.Fatalf("attempt %d waits %s", attempt, d)
		}
	}
	if reconnectDelay(1) > 2*time.Second {
		t.Fatal("the first retry should be quick")
	}
}

// decodeEvent is exercised through the wire format the socket actually carries.
func TestStreamIgnoresUnknownFrames(t *testing.T) {
	frames := []string{rawGreeting, `{"hello":true}`, `[]`, ``}
	for _, f := range frames {
		if _, ok := DecodeEvent([]byte(f)); ok {
			t.Fatalf("%q decoded as an event", f)
		}
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(rawGreeting), &envelope); err != nil {
		t.Fatal(err)
	}
	if _, ok := envelope["eventId"]; ok {
		t.Fatal("the greeting has an eventId after all; the guard needs revisiting")
	}
}
