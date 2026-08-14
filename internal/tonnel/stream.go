package tonnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const (
	// replayPage is the largest page the replay endpoint allows.
	replayPage = 500
	// maxReplayPages bounds the catch-up after a long outage. Seven days of
	// retention at this page size is far more than the desk can usefully act
	// on; the point of replay is to close a gap, not to import history.
	maxReplayPages = 400
	// maxLiveBuffer is how many live events may pile up while replay runs. An
	// overflow means the gap is being refilled slower than the market moves, so
	// the session restarts and replays again rather than silently skipping.
	maxLiveBuffer = 20000
	// seenCapacity is the deduplication window. Reconnect recovery re-delivers
	// events already seen live, and this is what makes processing idempotent
	// without asking the database.
	seenCapacity = 50000
	// streamStale is the backstop for a socket that is open but silently dead.
	// It is deliberately far longer than any plausible quiet spell: the real
	// liveness check is the ping below, which catches a broken pipe inside a
	// minute. Measured on the live feed, quiet hours run to one event every
	// fifteen seconds or so, and a shorter window here would hand the market
	// back to the poller — the exact traffic this whole path exists to avoid —
	// every time the marketplace took a breath.
	streamStale = 15 * time.Minute
	// cursorSaveEvery throttles cursor persistence. The contract is to save
	// only after successful processing; it does not require a write per event.
	cursorSaveEvery = 3 * time.Second
)

// StreamOptions configures the marketplace event consumer.
type StreamOptions struct {
	// Host serves both the socket and the replay endpoint. Empty means
	// EventHost.
	Host string
	// HTTP is used for the replay pages and the socket handshake.
	HTTP *http.Client

	// LoadCursor and SaveCursor persist the last fully processed eventId across
	// restarts. Both are optional: without them the stream simply starts live.
	LoadCursor func(context.Context) (string, error)
	SaveCursor func(context.Context, string) error

	// Handle processes one event. It must be quick — the socket is read by the
	// same goroutine, so slow handling becomes backpressure and eventually a
	// server-side disconnect. Returning an error leaves the cursor where it was
	// so the event is redelivered by the next replay.
	Handle func(context.Context, Event) error

	// OnUp fires when a session is live and caught up; OnDown when it drops.
	OnUp   func(replayed int)
	OnDown func(err error)
}

// Stream consumes the Tonnel marketplace event API: live over a WebSocket, with
// seven-day replay to close whatever gap a disconnect opened.
type Stream struct {
	opts StreamOptions
	host string
	http *http.Client
	seen *seenSet

	mu     sync.Mutex
	cursor string
	saved  string

	connected  atomic.Bool
	connectAt  atomic.Int64
	lastEvent  atomic.Int64
	eventCount atomic.Int64
	deferred   atomic.Int64
	lastErr    atomic.Pointer[string]
}

// NewStream builds a consumer. It does nothing until Run is called.
func NewStream(o StreamOptions) *Stream {
	if o.Host == "" {
		o.Host = EventHost
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	return &Stream{
		opts: o,
		host: o.Host,
		http: o.HTTP,
		seen: newSeenSet(seenCapacity),
	}
}

// Run connects and keeps reconnecting until ctx is cancelled. It blocks.
func (s *Stream) Run(ctx context.Context) {
	if c, err := s.loadCursor(ctx); err == nil && c != "" {
		s.mu.Lock()
		s.cursor, s.saved = c, c
		s.mu.Unlock()
	}

	attempt := 0
	for ctx.Err() == nil {
		started := time.Now()
		err := s.session(ctx)
		if ctx.Err() != nil {
			return
		}
		s.connected.Store(false)
		s.flushCursor(ctx)

		if err != nil && !errors.Is(err, context.Canceled) {
			msg := err.Error()
			s.lastErr.Store(&msg)
			if s.opts.OnDown != nil {
				s.opts.OnDown(err)
			}
		}
		// A session that lasted is evidence the endpoint is healthy, so the next
		// failure starts backing off from scratch rather than from wherever the
		// previous outage left the counter.
		if time.Since(started) > 2*time.Minute {
			attempt = 0
		}
		attempt++
		select {
		case <-ctx.Done():
			return
		case <-time.After(reconnectDelay(attempt)):
		}
	}
}

// reconnectDelay is exponential backoff with jitter, as the contract asks.
func reconnectDelay(attempt int) time.Duration {
	base := time.Second * time.Duration(math.Pow(2, math.Min(float64(attempt-1), 6)))
	if base > time.Minute {
		base = time.Minute
	}
	return base + time.Duration(rand.Int63n(int64(base/2+1)))
}

// session runs one connection: dial, replay the gap, then live.
//
// The order is the race-free recovery the contract specifies. Live frames that
// arrive while replay is still paging are buffered rather than processed, then
// merged in occurredAt order once the gap is closed, so an event can never be
// applied before an older one that describes the same gift.
func (s *Stream) session(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	dialCtx, dialCancel := context.WithTimeout(ctx, 20*time.Second)
	conn, resp, err := websocket.Dial(dialCtx, s.wsURL(), &websocket.DialOptions{
		HTTPClient: s.http,
		HTTPHeader: http.Header{"User-Agent": {streamUserAgent}},
	})
	dialCancel()
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("marketplace ws: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(8 << 20)

	var (
		mu        sync.Mutex
		replaying = true
		buffered  []Event
	)

	readDone := make(chan error, 1)
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				readDone <- err
				return
			}
			ev, ok := DecodeEvent(data)
			if !ok {
				continue // the connected greeting, or a shape we do not know
			}
			mu.Lock()
			if replaying {
				if len(buffered) >= maxLiveBuffer {
					mu.Unlock()
					readDone <- errors.New("live buffer overflowed while replaying")
					return
				}
				buffered = append(buffered, ev)
				mu.Unlock()
				continue
			}
			mu.Unlock()
			if err := s.process(ctx, ev); err != nil {
				readDone <- err
				return
			}
		}
	}()

	replayed, err := s.replay(ctx)
	if err != nil {
		return err
	}

	// Drained under the lock: the reader must not overtake the backlog it is
	// still filling, and blocking it here is the backpressure that keeps the
	// merge honest.
	mu.Lock()
	sortByOccurredAt(buffered)
	for _, ev := range buffered {
		if err := s.process(ctx, ev); err != nil {
			mu.Unlock()
			return err
		}
	}
	buffered, replaying = nil, false
	mu.Unlock()

	s.connected.Store(true)
	s.connectAt.Store(time.Now().UnixNano())
	s.lastErr.Store(nil)
	if s.opts.OnUp != nil {
		s.opts.OnUp(replayed)
	}

	go s.keepalive(ctx, conn)
	go s.flushLoop(ctx)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-readDone:
		return err
	}
}

// sortByOccurredAt orders buffered live events the way the market produced
// them, so the backlog can be merged behind the replay without an event being
// applied before an older one about the same gift.
func sortByOccurredAt(evs []Event) {
	sort.SliceStable(evs, func(i, j int) bool {
		return evs[i].OccurredAt.Before(evs[j].OccurredAt.Time)
	})
}

// streamUserAgent identifies the desk on a public, documented endpoint. There is
// nothing to impersonate here: this API asks for no authentication and no
// browser, and a name is more useful to whoever reads Tonnel's logs than a
// borrowed Chrome string.
const streamUserAgent = "floorline/1.0 (+marketplace-events)"

// keepalive detects a half-open socket. The server pings every thirty seconds,
// which proves it is alive but not that our writes still reach it.
func (s *Stream) keepalive(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(45 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pingCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				conn.CloseNow() // unblocks the reader, which ends the session
				return
			}
		}
	}
}

// replay pages through everything persisted since the stored cursor.
//
// With no cursor there is nothing to recover and replay is skipped entirely:
// starting from the oldest retained event would mean importing seven days of
// marketplace activity to arrive at a live feed we could have joined at once.
func (s *Stream) replay(ctx context.Context) (int, error) {
	total := 0
	for page := 0; page < maxReplayPages; page++ {
		after := s.cursorValue()
		if after == "" {
			return total, nil
		}

		events, err := s.replayPage(ctx, after)
		if err != nil {
			if errors.Is(err, errCursorExpired) {
				// Older than retention. Everything in between is gone, so the
				// only honest move is to rejoin live and say so.
				s.setCursor("")
				return total, nil
			}
			return total, err
		}
		for _, ev := range events {
			if err := s.process(ctx, ev); err != nil {
				return total, err
			}
		}
		total += len(events)
		if len(events) < replayPage {
			return total, nil
		}
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
	}
	return total, nil
}

var errCursorExpired = errors.New("replay cursor expired")

// replayPage fetches one page.
//
// Deliberately unfiltered, though the endpoint offers a `types` parameter. The
// socket ignores the same parameter — verified live, it delivers auctions and
// buy offers to a connection that asked for four listing types — so filtering
// only the replay half would make the two disagree about what the cursor even
// points at. Everything arrives, and the handler ignores what it does not act
// on; the contract asks consumers to tolerate unknown types anyway.
func (s *Stream) replayPage(ctx context.Context, after string) ([]Event, error) {
	q := url.Values{}
	q.Set("limit", fmt.Sprint(replayPage))
	q.Set("after", after)
	endpoint := "https://" + s.host + "/api/marketplace/events?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", streamUserAgent)

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("marketplace replay: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("marketplace replay: read body: %w", err)
	}
	if resp.StatusCode == http.StatusBadRequest {
		return nil, errCursorExpired
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{Op: "/api/marketplace/events", Status: resp.StatusCode, Message: extractMessage(body)}
	}

	var page struct {
		Status string  `json:"status"`
		Events []Event `json:"events"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("marketplace replay: decode: %w", err)
	}
	return page.Events, nil
}

// process applies one event exactly once and advances the cursor behind it.
//
// A handler failure ends the session on purpose. The cursor has not moved past
// the event, and its id is forgotten, so the reconnect replays from there and
// delivers it again — which is the only recovery this API offers and the only
// one that keeps the tape whole. Carrying on instead would advance the cursor
// past the failure on the next event and lose it for good; losing a listing is
// a missed trade, losing a sale is a hole in every median computed from it.
func (s *Stream) process(ctx context.Context, ev Event) error {
	if !s.seen.add(ev.EventID) {
		return nil
	}
	s.lastEvent.Store(time.Now().UnixNano())
	s.eventCount.Add(1)

	if s.opts.Handle != nil {
		if err := s.opts.Handle(ctx, ev); err != nil {
			s.seen.forget(ev.EventID)
			s.deferred.Add(1)
			return fmt.Errorf("handle %s: %w", ev.Type, err)
		}
	}
	s.noteCursor(ctx, ev.EventID)
	return nil
}

func (s *Stream) wsURL() string { return "wss://" + s.host + "/api/marketplace/ws" }

func (s *Stream) loadCursor(ctx context.Context) (string, error) {
	if s.opts.LoadCursor == nil {
		return "", nil
	}
	return s.opts.LoadCursor(ctx)
}

func (s *Stream) cursorValue() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor
}

func (s *Stream) setCursor(v string) {
	s.mu.Lock()
	s.cursor = v
	s.mu.Unlock()
}

// noteCursor records progress in memory. Persisting it is the flush loop's job:
// writing per event would be a database round trip per listing on a market that
// posts several a second, and writing only on a threshold left the stored cursor
// permanently one event behind whenever the market went quiet.
func (s *Stream) noteCursor(_ context.Context, id string) {
	s.mu.Lock()
	s.cursor = id
	s.mu.Unlock()
}

// flushLoop persists the cursor on a timer for as long as the session lives.
func (s *Stream) flushLoop(ctx context.Context) {
	t := time.NewTicker(cursorSaveEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.flushCursor(ctx)
		}
	}
}

func (s *Stream) flushCursor(ctx context.Context) {
	if s.opts.SaveCursor == nil {
		return
	}
	s.mu.Lock()
	cur, saved := s.cursor, s.saved
	s.mu.Unlock()
	if cur == saved {
		return
	}
	// Detached from the session context on purpose: the last thing a dying
	// connection should do is record how far it got.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.opts.SaveCursor(saveCtx, cur); err == nil {
		s.mu.Lock()
		s.saved = cur
		s.mu.Unlock()
	}
}

// ---- health -------------------------------------------------------------

// Connected reports whether a session is currently live and caught up.
func (s *Stream) Connected() bool { return s.connected.Load() }

// Healthy reports whether the feed can be relied on right now — which is what
// decides whether the pollers it replaced spend a request. A dropped socket
// answers this immediately; a socket that is open but has heard nothing for a
// very long time is treated as broken too, since it cannot be told apart from
// one by looking.
func (s *Stream) Healthy() bool {
	if !s.connected.Load() {
		return false
	}
	last := s.LastEvent()
	if last.IsZero() {
		return time.Since(s.ConnectedAt()) < streamStale
	}
	return time.Since(last) < streamStale
}

// LastEvent is when the last event was processed.
func (s *Stream) LastEvent() time.Time { return nanos(s.lastEvent.Load()) }

// ConnectedAt is when the current session came up.
func (s *Stream) ConnectedAt() time.Time { return nanos(s.connectAt.Load()) }

// Events is how many events have been processed since start.
func (s *Stream) Events() int64 { return s.eventCount.Load() }

// Deferred is how many events a handler refused and left for the next replay.
func (s *Stream) Deferred() int64 { return s.deferred.Load() }

// LastError is the most recent session failure, or "".
func (s *Stream) LastError() string {
	if p := s.lastErr.Load(); p != nil {
		return *p
	}
	return ""
}

func nanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// ---- deduplication ------------------------------------------------------

// seenSet remembers recent event ids in arrival order, evicting the oldest once
// full. Reconnect recovery replays events already seen live, and the contract
// puts the burden of deduplication on the consumer.
type seenSet struct {
	mu    sync.Mutex
	max   int
	ids   map[string]struct{}
	order []string
}

func newSeenSet(max int) *seenSet {
	return &seenSet{max: max, ids: make(map[string]struct{}, max/4+1)}
}

// add records an id and reports whether it is new.
func (s *seenSet) add(id string) bool {
	if id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ids[id]; ok {
		return false
	}
	s.ids[id] = struct{}{}
	s.order = append(s.order, id)
	if len(s.order) > s.max {
		drop := s.order[0]
		s.order = s.order[1:]
		delete(s.ids, drop)
	}
	return true
}

// forget removes an id so a later replay may deliver it again.
func (s *seenSet) forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ids[id]; !ok {
		return
	}
	delete(s.ids, id)
	for i, v := range s.order {
		if v == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}
