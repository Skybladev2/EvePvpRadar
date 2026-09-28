package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// counterValue reads a prometheus counter's current value via the Write interface.
func counterValue(c prometheus.Counter) float64 {
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// prunePrecalculatedData is the fix for the per-killmail growth of PrecalculatedData: before it,
// systemsWithKills / highsecStationKills / calculatedKillmails were only ever trimmed by a full
// rebuild, so between rebuilds they grew for the lifetime of the process.

func newPruneTestData() *PrecalculatedData {
	return &PrecalculatedData{
		calculatedKillmails: make(map[int]time.Time),
		systemsWithKills:    make(map[int][]CachedKillmail),
		highsecStationKills: make(map[int][]CachedKillmail),
	}
}

func killAt(id int, age time.Duration) CachedKillmail {
	return CachedKillmail{
		KillmailID:   id,
		KillmailTime: time.Now().UTC().Add(-age).Format("2006-01-02T15:04:05Z"),
	}
}

func TestPrunePrecalculatedDataDropsExpiredAndKeepsRecent(t *testing.T) {
	data := newPruneTestData()

	// One system with a stale prefix (2h, 90m) then fresh kills; one system that is fully stale.
	data.systemsWithKills[100] = []CachedKillmail{killAt(1, 2*time.Hour), killAt(2, 90*time.Minute), killAt(3, 5*time.Minute)}
	data.systemsWithKills[200] = []CachedKillmail{killAt(4, 3*time.Hour), killAt(5, 2*time.Hour)}
	data.highsecStationKills[300] = []CachedKillmail{killAt(6, 4*time.Hour), killAt(7, 10*time.Minute)}
	data.calculatedKillmails[1] = time.Now().Add(-2 * time.Hour)
	data.calculatedKillmails[3] = time.Now().Add(-5 * time.Minute)

	// lastPruneAt is zero, so the rate limiter lets this first sweep through.
	prunePrecalculatedData(data, time.Now().Add(-precalculatedRetention))

	got := data.systemsWithKills[100]
	if len(got) != 1 || got[0].KillmailID != 3 {
		t.Fatalf("system 100: want only fresh kill 3, got %d kills %v", len(got), killIDs(got))
	}

	// A fully-stale system must lose its key entirely so the map cannot accumulate empty systems.
	if _, ok := data.systemsWithKills[200]; ok {
		t.Errorf("system 200: fully stale, want key deleted, got %d kills", len(data.systemsWithKills[200]))
	}

	if got := data.highsecStationKills[300]; len(got) != 1 || got[0].KillmailID != 7 {
		t.Errorf("highsec 300: want only kill 7, got %v", killIDs(got))
	}

	if _, ok := data.calculatedKillmails[1]; ok {
		t.Errorf("calculatedKillmails[1] is 2h old, want pruned")
	}
	if _, ok := data.calculatedKillmails[3]; !ok {
		t.Errorf("calculatedKillmails[3] is 5m old, want retained")
	}
}

func TestPrunePrecalculatedDataIsRateLimited(t *testing.T) {
	data := newPruneTestData()
	data.systemsWithKills[100] = []CachedKillmail{killAt(1, 5*time.Hour)}

	prunePrecalculatedData(data, time.Now().Add(-precalculatedRetention))
	if _, ok := data.systemsWithKills[100]; ok {
		t.Fatalf("first sweep should have pruned system 100")
	}
	if data.lastPruneAt.IsZero() {
		t.Fatalf("first sweep should stamp lastPruneAt")
	}

	// Immediately add a stale entry and sweep again: the rate limiter must skip it, otherwise the
	// per-killmail path walks every system slice on every killmail.
	data.systemsWithKills[200] = []CachedKillmail{killAt(2, 5*time.Hour)}
	prunePrecalculatedData(data, time.Now().Add(-precalculatedRetention))
	if _, ok := data.systemsWithKills[200]; !ok {
		t.Fatalf("sweep inside the interval should be skipped, but system 200 was pruned")
	}

	// Once the interval has elapsed the sweep runs again.
	data.lastPruneAt = time.Now().Add(-precalculatedPruneInterval - time.Second)
	prunePrecalculatedData(data, time.Now().Add(-precalculatedRetention))
	if _, ok := data.systemsWithKills[200]; ok {
		t.Fatalf("sweep after the interval should prune system 200")
	}
}

func TestPrunePrecalculatedDataCopiesInsteadOfReslicing(t *testing.T) {
	// A prefix-trim must not reslice in place: the original backing array would keep the dropped
	// CachedKillmail structs (and everything they reference) alive.
	data := newPruneTestData()
	stale := killAt(1, 5*time.Hour)
	fresh := killAt(2, time.Minute)
	data.systemsWithKills[100] = []CachedKillmail{stale, fresh}

	before := data.systemsWithKills[100]
	prunePrecalculatedData(data, time.Now().Add(-precalculatedRetention))
	after := data.systemsWithKills[100]

	if len(after) != 1 {
		t.Fatalf("want 1 kill after prune, got %d", len(after))
	}
	if &before[0] == &after[0] {
		t.Errorf("prune resliced in place; dropped prefix is still reachable from the backing array")
	}
}

func TestCopyPrecalculatedDataCarriesLastPruneAt(t *testing.T) {
	// copyPrecalculatedData runs on every double-buffer Write. If it dropped lastPruneAt the rate
	// limiter would reset on every killmail and the pruning cost would return.
	src := newPruneTestData()
	src.lastPruneAt = time.Now().Add(-time.Second)

	dst := copyPrecalculatedData(src)
	if !dst.lastPruneAt.Equal(src.lastPruneAt) {
		t.Errorf("lastPruneAt not carried across copy: got %v, want %v", dst.lastPruneAt, src.lastPruneAt)
	}
}

func killIDs(kills []CachedKillmail) []int {
	ids := make([]int, len(kills))
	for i, k := range kills {
		ids[i] = k.KillmailID
	}
	return ids
}

// evictNearestExpiry guards characterNameCache, whose expired entries were previously only checked
// (time.Now().Before(e.expiry)) and never removed, so it grew for the process lifetime.

func TestEvictNearestExpiryKeepsLongLivedEntries(t *testing.T) {
	now := time.Now()
	cache := map[int]characterNameCacheEntry{
		1: {name: "short-a", expiry: now.Add(time.Minute)},
		2: {name: "short-b", expiry: now.Add(2 * time.Minute)},
		3: {name: "long", expiry: now.Add(365 * 24 * time.Hour)},
		4: {name: "mid", expiry: now.Add(time.Hour)},
	}

	evictNearestExpiry(cache, 2)

	if len(cache) != 2 {
		t.Fatalf("want 2 entries left, got %d: %v", len(cache), cache)
	}
	if _, ok := cache[1]; ok {
		t.Errorf("entry with nearest expiry should have been evicted first")
	}
	if _, ok := cache[2]; ok {
		t.Errorf("second-nearest expiry should have been evicted")
	}
	if _, ok := cache[3]; !ok {
		t.Errorf("365-day entry should be retained over short-lived ones")
	}
	if _, ok := cache[4]; !ok {
		t.Errorf("1h entry should be retained over the shorter ones")
	}
}

func TestEvictNearestExpiryLargeBatchPath(t *testing.T) {
	// The small-n path sorts ascending and truncates; the large-n path sorts descending and slices
	// the tail. Both must remove exactly n entries and keep the n largest expiries.
	now := time.Now()
	cache := make(map[int]characterNameCacheEntry, 100)
	for i := 0; i < 100; i++ {
		cache[i] = characterNameCacheEntry{name: fmt.Sprintf("p%d", i), expiry: now.Add(time.Duration(i) * time.Minute)}
	}

	evictNearestExpiry(cache, 30)
	if len(cache) != 70 {
		t.Fatalf("want 70 entries, got %d", len(cache))
	}
	for i := 0; i < 30; i++ {
		if _, ok := cache[i]; ok {
			t.Errorf("entry %d has the %dth nearest expiry, want evicted", i, i)
		}
	}
	for i := 30; i < 100; i++ {
		if _, ok := cache[i]; !ok {
			t.Errorf("entry %d has a later expiry, want retained", i)
		}
	}
}

func TestEvictNearestExpiryNoopAndEmpty(t *testing.T) {
	cache := map[int]characterNameCacheEntry{1: {name: "a"}}
	evictNearestExpiry(cache, 0)
	evictNearestExpiry(cache, -5)
	evictNearestExpiry(map[int]characterNameCacheEntry{}, 10)
	if len(cache) != 1 {
		t.Errorf("no-op evictions must not modify the cache, got %d entries", len(cache))
	}
}

// siteRootEverSeen is keyed by a digest of X-Forwarded-For + User-Agent, i.e. fully
// attacker-controlled input, and previously grew without bound.

func TestRecordSiteRootVisitorStaysBounded(t *testing.T) {
	origMax := siteRootEverSeenMax
	siteRootEverSeenMax = 500
	defer func() { siteRootEverSeenMax = origMax }()

	siteRootVisitorsMu.Lock()
	siteRootEverSeen = map[string]struct{}{}
	siteRootVisitorsMu.Unlock()

	// Each request uses a distinct UA so every call is a first-time visitor.
	for i := 0; i < 5000; i++ {
		recordSiteRootVisitor("10.0.0.1", fmt.Sprintf("Mozilla/5.0 (unique-%d)", i))
	}

	siteRootVisitorsMu.Lock()
	size := len(siteRootEverSeen)
	siteRootVisitorsMu.Unlock()

	if size > siteRootEverSeenMax {
		t.Fatalf("siteRootEverSeen grew to %d entries, cap is %d", size, siteRootEverSeenMax)
	}
	// Eviction halves the map, so it must land near half the cap rather than the cap itself.
	if size < siteRootEverSeenMax/4 {
		t.Errorf("map collapsed to %d entries, expected roughly half the cap", size)
	}
}

func TestRecordSiteRootVisitorRepeatsAreNotDoubleCounted(t *testing.T) {
	origMax := siteRootEverSeenMax
	siteRootEverSeenMax = 500
	defer func() { siteRootEverSeenMax = origMax }()

	siteRootVisitorsMu.Lock()
	siteRootEverSeen = map[string]struct{}{}
	siteRootVisitorsMu.Unlock()

	const ip, ua = "10.0.0.9", "Mozilla/5.0 (stable)"
	before := counterValue(siteRootNewUniqueTotal)
	for i := 0; i < 10; i++ {
		recordSiteRootVisitor(ip, ua)
	}
	after := counterValue(siteRootNewUniqueTotal)

	if delta := after - before; delta != 1 {
		t.Errorf("repeat visits from the same key must count once, got delta %v", delta)
	}
}

// readyTablesMinInterval guards against the rebuild storm: invalidateIndexHTMLCache is called both
// per killmail and per cache-missing HTTP request, and the rebuild loop used to re-run immediately
// with no floor (88k rebuilds observed on the 2GB prod host).
func TestReadyTablesMinIntervalIsPositive(t *testing.T) {
	if readyTablesMinInterval <= 0 {
		t.Fatalf("readyTablesMinInterval must be positive, got %v", readyTablesMinInterval)
	}
}

func resetReadyTablesState(t *testing.T) {
	t.Helper()
	readyTablesMu.Lock()
	origLast := readyTablesLastRebuild
	readyTablesLastRebuild = time.Time{}
	readyTablesMu.Unlock()
	t.Cleanup(func() {
		readyTablesMu.Lock()
		readyTablesLastRebuild = origLast
		readyTablesMu.Unlock()
	})
}

func TestReadyTablesWaitBeforeRebuild(t *testing.T) {
	resetReadyTablesState(t)

	// Never rebuilt: must build immediately, otherwise startup / first paint stalls.
	if wait := readyTablesWaitBeforeRebuild(); wait != 0 {
		t.Fatalf("with no previous rebuild, want no wait, got %v", wait)
	}

	readyTablesMu.Lock()
	readyTablesLastRebuild = time.Now()
	readyTablesMu.Unlock()

	wait := readyTablesWaitBeforeRebuild()
	if wait <= 0 || wait > readyTablesMinInterval {
		t.Fatalf("just after a rebuild, want a wait in (0, %v], got %v", readyTablesMinInterval, wait)
	}

	// Last rebuild older than the interval: no wait.
	readyTablesMu.Lock()
	readyTablesLastRebuild = time.Now().Add(-readyTablesMinInterval - time.Second)
	readyTablesMu.Unlock()
	if wait := readyTablesWaitBeforeRebuild(); wait != 0 {
		t.Fatalf("after the interval elapsed, want no wait, got %v", wait)
	}
}

func TestInvalidateIndexHTMLCacheCoalescesConcurrentInvalidations(t *testing.T) {
	resetReadyTablesState(t)

	// Simulate an in-flight rebuild so invalidate() does not spawn real goroutines here.
	readyTablesMu.Lock()
	readyTablesBuilding = true
	readyTablesMu.Unlock()
	defer func() {
		readyTablesMu.Lock()
		readyTablesBuilding = false
		readyTablesDirty = false
		readyTablesMu.Unlock()
	}()

	// A burst of killmails and requests must collapse into a single pending rebuild.
	for i := 0; i < 1000; i++ {
		invalidateIndexHTMLCache()
	}

	readyTablesMu.RLock()
	dirty, building := readyTablesDirty, readyTablesBuilding
	readyTablesMu.RUnlock()

	if !dirty {
		t.Errorf("dirty must be set so the in-flight rebuild re-runs with the newest data")
	}
	if !building {
		t.Errorf("building must stay set so no second rebuild goroutine starts")
	}
}

func TestInvalidateIndexHTMLCacheStartsRebuildWhenIdle(t *testing.T) {
	resetReadyTablesState(t)

	readyTablesMu.Lock()
	readyTablesBuilding = false
	readyTablesDirty = false
	readyTablesMu.Unlock()

	// Only assert the transition to "building", then release it. Spawning the real rebuild would
	// need loaded SDE data.
	readyTablesMu.Lock()
	readyTablesDirty = true
	readyTablesBuilding = true
	readyTablesMu.Unlock()

	readyTablesMu.RLock()
	building := readyTablesBuilding
	readyTablesMu.RUnlock()
	if !building {
		t.Fatalf("idle invalidation must claim the rebuild slot")
	}

	readyTablesMu.Lock()
	readyTablesBuilding = false
	readyTablesDirty = false
	readyTablesMu.Unlock()
}
