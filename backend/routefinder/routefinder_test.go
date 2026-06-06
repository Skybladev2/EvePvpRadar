package routefinder

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// LoadSystemsFromSDE loads systems from SDE files (test helper)
func loadSystemsForTest() ([]System, error) {
	// Try to load from generated/systems.json first
	if file, err := os.Open(filepath.Join("..", "generated", "systems.json")); err == nil {
		defer file.Close()

		decoder := json.NewDecoder(file)
		var systems []System
		err = decoder.Decode(&systems)
		if err == nil && len(systems) > 0 {
			return systems, nil
		}
	}

	// Try to load from SDE
	systemsFile := filepath.Join("..", "sde", "mapSolarSystems.jsonl")
	if _, err := os.Stat(systemsFile); os.IsNotExist(err) {
		return nil, fmt.Errorf("systems file not found: %s", systemsFile)
	}

	stargatesFile := filepath.Join("..", "sde", "mapStargates.jsonl")
	if _, err := os.Stat(stargatesFile); os.IsNotExist(err) {
		return nil, fmt.Errorf("stargates file not found: %s", stargatesFile)
	}

	systems, err := loadSystemsFromSDEFiles(systemsFile, stargatesFile)
	if err != nil {
		return nil, err
	}

	return systems, nil
}

func loadSystemsFromSDEFiles(systemsFile, stargatesFile string) ([]System, error) {
	// Load systems
	systemsContent, err := os.ReadFile(systemsFile)
	if err != nil {
		return nil, err
	}

	systems := make(map[int]System)
	lines := strings.Split(string(systemsContent), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var systemData map[string]interface{}
		if err := json.Unmarshal([]byte(line), &systemData); err != nil {
			continue
		}

		nameObj, ok := systemData["name"].(map[string]interface{})
		if !ok {
			continue
		}
		systemName, ok := nameObj["en"].(string)
		if !ok {
			continue
		}

		systemID, ok := systemData["_key"].(float64)
		if !ok {
			continue
		}

		securityStatus, ok := systemData["securityStatus"].(float64)
		if !ok {
			securityStatus = 0.0
		}

		systems[int(systemID)] = System{
			SystemID:   int(systemID),
			SystemName: systemName,
			Security:   securityStatus,
			Stargates:  []Stargate{},
		}
	}

	// Load stargates
	stargatesContent, err := os.ReadFile(stargatesFile)
	if err != nil {
		return nil, err
	}

	lines = strings.Split(string(stargatesContent), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var stargateData map[string]interface{}
		if err := json.Unmarshal([]byte(line), &stargateData); err != nil {
			continue
		}

		stargateID, ok := stargateData["_key"].(float64)
		if !ok {
			continue
		}

		fromSystem, ok := stargateData["solarSystemID"].(float64)
		if !ok {
			continue
		}

		destination, ok := stargateData["destination"].(map[string]interface{})
		if !ok {
			continue
		}

		toSystem, ok := destination["solarSystemID"].(float64)
		if !ok {
			continue
		}

		var position [3]float64
		if posObj, ok := stargateData["position"].(map[string]interface{}); ok {
			if x, ok := posObj["x"].(float64); ok {
				position[0] = x
			}
			if y, ok := posObj["y"].(float64); ok {
				position[1] = y
			}
			if z, ok := posObj["z"].(float64); ok {
				position[2] = z
			}
		}

		systemID := int(fromSystem)
		if sys, exists := systems[systemID]; exists {
			sys.Stargates = append(sys.Stargates, Stargate{
				ID:                    int(stargateID),
				Position:              position,
				DestinationStargateID: int(toSystem), // Actually destination system ID
			})
			systems[systemID] = sys
		}
	}

	// Convert map to slice
	result := make([]System, 0, len(systems))
	for _, sys := range systems {
		result = append(result, sys)
	}

	return result, nil
}

func TestRouteFinder_Performance(t *testing.T) {
	// Load systems first (without triggering main package startup)
	systems, err := loadSystemsForTest()
	if err != nil {
		t.Fatalf("Failed to load systems: %v", err)
	}

	// Initialize route finder after systems are loaded
	rf := NewRouteFinder(systems)

	// Test cases: pairs of system IDs that should be reachable
	testCases := []struct {
		name         string
		fromSystemID int
		toSystemID   int
		maxTime      time.Duration
	}{
		{"Jita to Amarr", 30000142, 30002187, 10 * time.Millisecond},
		{"Jita to Rens", 30000142, 30002510, 10 * time.Millisecond},
		{"Amarr to Dodixie", 30002187, 30002659, 10 * time.Millisecond},
		{"Hek to Jita", 30002053, 30000142, 10 * time.Millisecond},
		{"Same system", 30000142, 30000142, 1 * time.Millisecond},
		// 3-QYVE to 2-RSC7
		{"Distant systems", 30003675, 30004492, 10 * time.Millisecond},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Run multiple iterations for more accurate timing
			const iterations = 100
			var totalDuration time.Duration
			var route *Route
			var err error

			for i := 0; i < iterations; i++ {
				start := time.Now()
				route, err = rf.FindShortestRoute(tc.fromSystemID, tc.toSystemID, 0)
				totalDuration += time.Since(start)
			}
			avgDuration := totalDuration / iterations

			if err != nil {
				t.Logf("Route not found (this is OK for some test cases): %v", err)
				return
			}

			if avgDuration > tc.maxTime {
				t.Errorf("Route finding took %v (avg over %d iterations), expected < %v", avgDuration, iterations, tc.maxTime)
			}

			if route.FromSystemID != tc.fromSystemID {
				t.Errorf("Expected fromSystemID %d, got %d", tc.fromSystemID, route.FromSystemID)
			}

			if route.ToSystemID != tc.toSystemID {
				t.Errorf("Expected toSystemID %d, got %d", tc.toSystemID, route.ToSystemID)
			}

			if len(route.Path) < 2 && tc.fromSystemID != tc.toSystemID {
				t.Errorf("Expected path with at least 2 systems, got %d", len(route.Path))
			}

			if route.Path[0] != tc.fromSystemID {
				t.Errorf("Path should start with fromSystemID %d, got %d", tc.fromSystemID, route.Path[0])
			}

			if route.Path[len(route.Path)-1] != tc.toSystemID {
				t.Errorf("Path should end with toSystemID %d, got %d", tc.toSystemID, route.Path[len(route.Path)-1])
			}

			if route.Jumps != len(route.Path)-1 {
				t.Errorf("Jumps should be %d, got %d", len(route.Path)-1, route.Jumps)
			}

			// Format duration with better precision
			nanos := avgDuration.Nanoseconds()
			var durationStr string
			if nanos < 1000 {
				durationStr = fmt.Sprintf("%d ns", nanos)
			} else if nanos < 1000000 {
				durationStr = fmt.Sprintf("%.2f μs", float64(nanos)/1000.0)
			} else if nanos < 1000000000 {
				durationStr = fmt.Sprintf("%.3f ms", float64(nanos)/1000000.0)
			} else {
				durationStr = fmt.Sprintf("%.3f s", float64(nanos)/1000000000.0)
			}

			t.Logf("Route found: %d jumps in %s (avg over %d iterations, total: %v)", route.Jumps, durationStr, iterations, totalDuration)
		})
	}
}

func TestRouteFinder_PerformanceBenchmark(t *testing.T) {
	// Load systems first
	systems, err := loadSystemsForTest()
	if err != nil {
		t.Fatalf("Failed to load systems: %v", err)
	}

	rf := NewRouteFinder(systems)

	// Benchmark: find routes to multiple destinations from Jita
	fromSystemID := 30000142 // Jita
	destinations := []int{
		30002187, // Amarr
		30002510, // Rens
		30002659, // Dodixie
		30002053, // Hek
		30004238, // Tash-Murkon Prime
	}

	// Run multiple iterations for more accurate timing
	const iterations = 100
	totalDuration := time.Duration(0)
	successfulRoutes := 0

	for i := 0; i < iterations; i++ {
		start := time.Now()
		for _, destID := range destinations {
			_, err := rf.FindShortestRoute(fromSystemID, destID, 0)
			if err == nil {
				successfulRoutes++
			}
		}
		totalDuration += time.Since(start)
	}

	avgTotalDuration := totalDuration / time.Duration(iterations)
	avgPerRoute := avgTotalDuration / time.Duration(len(destinations))

	if avgPerRoute > 10*time.Millisecond {
		t.Errorf("Average route finding took %v, expected < 10ms", avgPerRoute)
	}

	// Format with better precision
	nanos := avgPerRoute.Nanoseconds()
	var durationStr string
	if nanos < 1000 {
		durationStr = fmt.Sprintf("%d ns", nanos)
	} else if nanos < 1000000 {
		durationStr = fmt.Sprintf("%.2f μs", float64(nanos)/1000.0)
	} else if nanos < 1000000000 {
		durationStr = fmt.Sprintf("%.3f ms", float64(nanos)/1000000.0)
	} else {
		durationStr = fmt.Sprintf("%.3f s", float64(nanos)/1000000000.0)
	}

	t.Logf("Benchmark: Found %d routes over %d iterations, avg %s per route (total avg: %v for %d destinations)",
		successfulRoutes, iterations, durationStr, avgTotalDuration, len(destinations))
}

func TestRouteFinder_ConcurrentAccess(t *testing.T) {
	// Load systems first
	systems, err := loadSystemsForTest()
	if err != nil {
		t.Fatalf("Failed to load systems: %v", err)
	}

	rf := NewRouteFinder(systems)

	// Test concurrent access to route finder
	done := make(chan bool, 10)
	errors := make(chan error, 10)

	for i := 0; i < 10; i++ {
		go func() {
			_, err := rf.FindShortestRoute(30000142, 30002187, 0)
			if err != nil {
				errors <- err
			}
			done <- true
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		select {
		case <-done:
		case err := <-errors:
			t.Logf("Route not found (OK): %v", err)
		case <-time.After(1 * time.Second):
			t.Error("Timeout waiting for concurrent route finding")
		}
	}
}

func BenchmarkRouteFinder_FindShortestRoute(b *testing.B) {
	// Load systems first
	systems, err := loadSystemsForTest()
	if err != nil {
		b.Fatalf("Failed to load systems: %v", err)
	}

	rf := NewRouteFinder(systems)
	fromSystemID := 30000142 // Jita
	toSystemID := 30002187   // Amarr

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = rf.FindShortestRoute(fromSystemID, toSystemID, 0)
	}
}

func benchmarkDestinations(systems []System, fromSystemID, limit int) []int {
	destinations := make([]int, 0, limit)
	for _, sys := range systems {
		if sys.SystemID == fromSystemID || sys.SystemID == ZarzakhSystemID {
			continue
		}
		destinations = append(destinations, sys.SystemID)
		if len(destinations) >= limit {
			break
		}
	}
	return destinations
}

// Simulates old proximity behavior: run shortest-route BFS per destination.
func BenchmarkRouteFinder_ProximityPerTarget800(b *testing.B) {
	systems, err := loadSystemsForTest()
	if err != nil {
		b.Fatalf("Failed to load systems: %v", err)
	}
	rf := NewRouteFinder(systems)

	const fromSystemID = 30000142 // Jita
	const maxJumps = 15
	destinations := benchmarkDestinations(systems, fromSystemID, 800)
	if len(destinations) == 0 {
		b.Fatalf("No destinations available for benchmark")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, toSystemID := range destinations {
			_, _ = rf.FindShortestRoute(fromSystemID, toSystemID, maxJumps)
		}
	}
}

// Simulates optimized proximity behavior: one single-source BFS, then lookups/path rebuilds.
func BenchmarkRouteFinder_ProximitySingleSource800(b *testing.B) {
	systems, err := loadSystemsForTest()
	if err != nil {
		b.Fatalf("Failed to load systems: %v", err)
	}
	rf := NewRouteFinder(systems)

	const fromSystemID = 30000142 // Jita
	const maxJumps = 15
	destinations := benchmarkDestinations(systems, fromSystemID, 800)
	if len(destinations) == 0 {
		b.Fatalf("No destinations available for benchmark")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		paths := rf.FindShortestPathsFrom(fromSystemID, maxJumps)
		for _, toSystemID := range destinations {
			if _, ok := paths.Distances[toSystemID]; !ok {
				continue
			}
			_ = rf.BuildPath(paths, toSystemID)
		}
	}
}
