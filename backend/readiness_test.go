package main

import (
	"testing"

	zkillboardcache "evepvpsearch/zkillboard-cache"
)

// Readiness is a startup gate only: it must become ready once the initial backfill
// has finished and must not flip back to unready while a route recalculation runs,
// because responses are served from the current buffer.
func TestAppReadyForBalancerIgnoresRecalc(t *testing.T) {
	origCache := killmailCache
	origRecalc := recalcInProgress
	defer func() {
		killmailCache = origCache
		recalcInProgress = origRecalc
	}()

	killmailCache = nil
	if appReadyForBalancer() {
		t.Fatal("expected not ready without a killmail cache")
	}

	killmailCache = zkillboardcache.NewCache()
	if appReadyForBalancer() {
		t.Fatal("expected not ready before the initial backfill completes")
	}

	killmailCache.SetWarmupComplete()
	recalcInProgress = true
	if !appReadyForBalancer() {
		t.Fatal("expected ready after warmup even while a recalculation is in progress")
	}
}
