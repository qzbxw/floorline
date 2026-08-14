package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"floorline/internal/config"
	"floorline/internal/store"
	"floorline/internal/tonnel"
)

func coolingApp(t *testing.T) *App {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "cooling.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	api, err := tonnel.New(tonnel.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return &App{
		st:            st,
		api:           api,
		cfg:           &config.Config{},
		lastAPIOK:     func() time.Time { return time.Time{} },
		alertCooldown: make(map[string]time.Time),
		pollers:       make(map[string]*pollerState),
		collections:   make(map[string]struct{}),
	}
}

// A block has to actually stop the traffic. The desk used to announce that the
// pollers were slowing down and then keep every ticker running at full rate,
// which is what turned a five-minute challenge into hours of them.
func TestCoolingStopsThePollersItClaimsTo(t *testing.T) {
	a := coolingApp(t)
	a.coolUntil = time.Now().Add(10 * time.Minute)

	for _, name := range []string{"feed", "sales", "inventory", "scan"} {
		if a.mayPoll(name) {
			t.Fatalf("%s polled Tonnel while cooling", name)
		}
	}
	// The quote and the pruning have nothing to do with Tonnel and must survive
	// a block untouched.
	for _, name := range []string{"gram", "maintenance"} {
		if !a.mayPoll(name) {
			t.Fatalf("%s was stopped by a Tonnel block it has nothing to do with", name)
		}
	}
}

func TestCoolingLetsOneProbeThrough(t *testing.T) {
	a := coolingApp(t)
	a.coolUntil = time.Now().Add(10 * time.Minute)

	if !a.mayPoll(coolProbePoller) {
		t.Fatal("nothing was allowed to test whether the block had lifted")
	}
	if a.mayPoll(coolProbePoller) {
		t.Fatal("the probe repeated immediately; that is the hammering again")
	}

	a.mu.Lock()
	a.lastProbe = time.Now().Add(-coolProbeEvery - time.Second)
	a.mu.Unlock()
	if !a.mayPoll(coolProbePoller) {
		t.Fatal("the probe never came back around")
	}
}

func TestCoolingEndsWhenTheBlockDoes(t *testing.T) {
	a := coolingApp(t)
	a.coolUntil = time.Now().Add(-time.Second)
	for _, name := range []string{"feed", "stats", "sales", "inventory", "scan"} {
		if !a.mayPoll(name) {
			t.Fatalf("%s stayed stopped after the cooling window expired", name)
		}
	}
}

// The 14 Aug chat log: ⚠️ and ✅ alternating every thirty seconds for hours.
// Any poller finishing counted as recovery, including the GRAM quote, which
// reads a public exchange and never touches Tonnel at all.
func TestRecoveryNeedsTonnelItselfToAnswer(t *testing.T) {
	a := coolingApp(t)
	var sent []string
	a.notifier = func(s string) { sent = append(sent, s) }

	a.mu.Lock()
	a.blocked, a.blockAnnounced = true, true
	a.blockedAt = time.Now().Add(-time.Hour)
	a.lastBlockAt = time.Now().Add(-time.Second) // still being refused
	a.mu.Unlock()

	a.noteRecovered()
	if len(sent) != 0 {
		t.Fatalf("announced recovery while the refusals were still arriving: %v", sent)
	}

	// The refusals stopped a while ago, but Tonnel has still never answered.
	a.mu.Lock()
	a.lastBlockAt = time.Now().Add(-blockRecoveryHold - time.Minute)
	a.mu.Unlock()
	a.noteRecovered()
	if len(sent) != 0 {
		t.Fatalf("announced recovery without a single successful Tonnel call: %v", sent)
	}
}

// And once it does answer, and has kept answering, say so exactly once.
func TestRecoveryIsAnnouncedOnceTheDeskIsReallyBack(t *testing.T) {
	a := coolingApp(t)
	var sent []string
	a.notifier = func(s string) { sent = append(sent, s) }

	a.mu.Lock()
	a.blocked, a.blockAnnounced = true, true
	a.blockedAt = time.Now().Add(-time.Hour)
	a.lastBlockAt = time.Now().Add(-blockRecoveryHold - time.Minute)
	a.coolUntil = time.Now().Add(time.Hour)
	a.mu.Unlock()

	a.lastAPIOK = time.Now // a probe got through

	a.noteRecovered()
	if len(sent) != 1 {
		t.Fatalf("expected one all-clear, got %v", sent)
	}
	a.noteRecovered()
	if len(sent) != 1 {
		t.Fatalf("the all-clear repeated: %v", sent)
	}
	if a.cooling() {
		t.Fatal("still cooling after Tonnel came back")
	}
}

// A block nobody was told about must not produce an all-clear out of nowhere.
func TestSilentBlockClearsSilently(t *testing.T) {
	a := coolingApp(t)
	var sent []string
	a.notifier = func(s string) { sent = append(sent, s) }

	a.mu.Lock()
	a.blocked, a.blockAnnounced = true, false
	a.blockedAt = time.Now().Add(-time.Hour)
	a.lastBlockAt = time.Now().Add(-blockRecoveryHold - time.Minute)
	a.mu.Unlock()
	a.lastAPIOK = time.Now

	a.noteRecovered()
	if len(sent) != 0 {
		t.Fatalf("announced the end of a block that was never announced: %v", sent)
	}
}

// Streamed listings are still recorded while cooling — they cost nothing and
// the crowd measurements need them — but nothing is priced, because pricing
// reads the order book that is being refused.
func TestCoolingStoresListingsWithoutPricingThem(t *testing.T) {
	a := coolingApp(t)
	a.coolUntil = time.Now().Add(time.Hour)

	ctx := context.Background()
	g := tonnel.Gift{GiftID: 42, GiftNum: 7, Name: "Lol Pop", Model: "Blood Sucker (1%)", Price: 3, Asset: tonnel.AssetGRAM}
	if _, err := a.st.UpsertListings(ctx, []tonnel.Gift{g}, time.Now()); err != nil {
		t.Fatal(err)
	}
	// With no detector wired, evaluating would panic. Reaching the end proves
	// the cooling guard returned first.
	a.evaluateListings(ctx, []tonnel.Gift{g}, []int64{42}, time.Now())

	if seen, err := a.st.ListingFirstSeen(ctx, 42); err != nil || seen.IsZero() {
		t.Fatalf("listing was not stored: %v %v", seen, err)
	}
}

// While the free address is refused every request is being paid for by the
// byte, and the snapshot alone is 80 KB. The pollers keep their own floor on
// how often they may spend money — without it a 5 GB residential plan lasts
// about a month, and only if nothing else ever happens.
func TestFrugalModeSlowsTheExpensivePollers(t *testing.T) {
	a := coolingApp(t)
	paid := true
	a.metered = func() bool { return paid }

	now := time.Now()
	a.mu.Lock()
	for name := range frugalEvery {
		a.pollers[name] = &pollerState{LastRun: now.Add(-30 * time.Second)}
	}
	a.mu.Unlock()

	for name, every := range frugalEvery {
		if a.mayPoll(name) {
			t.Fatalf("%s polled 30s after its last run while paying (floor %s)", name, every)
		}
	}

	// The floor is a floor, not a stop: once it has passed, the poll runs.
	a.mu.Lock()
	for name := range frugalEvery {
		a.pollers[name].LastRun = now.Add(-time.Hour)
	}
	a.mu.Unlock()
	for name := range frugalEvery {
		if !a.mayPoll(name) {
			t.Fatalf("%s never came back around", name)
		}
	}

	// And on the free route nothing is throttled at all: freshness is only
	// traded away while it costs money.
	paid = false
	a.mu.Lock()
	for name := range frugalEvery {
		a.pollers[name].LastRun = now
	}
	a.mu.Unlock()
	for name := range frugalEvery {
		if !a.mayPoll(name) {
			t.Fatalf("%s was throttled while the traffic was free", name)
		}
	}
}

// The snapshot is the expensive one, and the ceiling on how stale it may get is
// not a matter of taste: unattended buying refuses data older than
// AUTOBUY_MAX_DATA_AGE, so a frugal interval above that would quietly disarm the
// desk instead of saving it money.
func TestTheFrugalSnapshotStaysInsideTheAutobuyDataAge(t *testing.T) {
	var cfg config.Config
	cfg.Auto.MaxDataAge = 5 * time.Minute // the shipped default
	if frugalEvery["stats"] >= cfg.Auto.MaxDataAge {
		t.Fatalf("frugal snapshot every %s against a %s data-age gate: autobuy would never fire",
			frugalEvery["stats"], cfg.Auto.MaxDataAge)
	}
}
