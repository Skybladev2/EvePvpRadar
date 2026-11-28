package routefinder

import "testing"

func TestRebuildGraphPreservesTheraConnections(t *testing.T) {
	systems := []System{
		{SystemID: TheraSystemID, SystemName: "Thera"},
		{SystemID: 30000142, SystemName: "Jita"},
		{SystemID: 30000144, SystemName: "Perimeter"},
	}

	rf := NewRouteFinder(systems)
	rf.SetMockTheraSignaturesWithWhType(map[int]string{
		30000142: "JIT-001",
		30000144: "PER-001",
	}, nil, nil)

	before, err := rf.FindShortestRoute(30000142, 30000144, 5)
	if err != nil {
		t.Fatalf("route before rebuild: %v", err)
	}
	if before.Jumps != 2 {
		t.Fatalf("expected 2 jumps through Thera before rebuild, got %d (%v)", before.Jumps, before.Path)
	}

	rf.RebuildGraph(systems)

	if got := rf.GetTheraSignaturesCount(); got != 2 {
		t.Fatalf("expected 2 Thera signatures preserved after rebuild, got %d", got)
	}

	after, err := rf.FindShortestRoute(30000142, 30000144, 5)
	if err != nil {
		t.Fatalf("route after rebuild: %v", err)
	}
	if after.Jumps != 2 {
		t.Fatalf("expected 2 jumps through Thera after rebuild, got %d (%v)", after.Jumps, after.Path)
	}
}
