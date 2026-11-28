package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	// sdeStargateRefreshInterval is how often the running application re-checks
	// the SDE for a changed stargate layout. The SDE index and table versions are
	// checked on every tick; unchanged tables are not downloaded again.
	sdeStargateRefreshInterval = 10 * time.Minute

	generatedSystemsPath = "generated/systems.json"
)

// startStargateLayoutRefresher periodically refreshes the stargate graph from
// the SDE. The first check runs one interval after application start.
func startStargateLayoutRefresher() {
	if mockData {
		log.Printf("SDE stargate refresher disabled in development mode")
		return
	}

	go func() {
		ticker := time.NewTicker(sdeStargateRefreshInterval)
		defer ticker.Stop()

		log.Printf("SDE stargate refresher started (interval %v)", sdeStargateRefreshInterval)
		for range ticker.C {
			if err := refreshStargateLayout(); err != nil {
				log.Printf("SDE stargate refresh failed: %v", err)
			}
		}
	}()
}

// refreshStargateLayout re-checks the SDE and, when the stargate layout changed,
// swaps the systems snapshot and rebuilds the route-finder graph.
func refreshStargateLayout() error {
	newSystems, err := refreshSystemsFromSDE()
	if err != nil {
		return err
	}
	if len(newSystems) == 0 {
		return errors.New("sde refresh returned no systems")
	}

	oldSystems := currentSystems()
	oldFingerprint := stargateLayoutFingerprint(oldSystems)
	newFingerprint := stargateLayoutFingerprint(newSystems)
	if oldFingerprint == newFingerprint {
		log.Printf("SDE stargate layout unchanged (%d systems)", len(newSystems))
		return nil
	}

	if globalRouteFinder != nil {
		globalRouteFinder.RebuildGraph(convertSystemsToRoutefinder(newSystems))
	}
	setSystems(newSystems)
	routeLayoutGeneration.Add(1)

	if err := writeGeneratedSystems(newSystems); err != nil {
		log.Printf("Warning: failed to update %s after SDE refresh: %v", generatedSystemsPath, err)
	}

	log.Printf("SDE stargate layout changed; route data updated with %d systems (%s -> %s)",
		len(newSystems), shortFingerprint(oldFingerprint), shortFingerprint(newFingerprint))
	recalculateForRouteDataUpdate("SDE stargate layout updated")
	return nil
}

// stargateLayoutFingerprint returns a stable digest of every system's stargate
// links. Stargate order in the SDE is irrelevant, so both systems and their
// gates are sorted before hashing.
func stargateLayoutFingerprint(list []System) string {
	sortedSystems := make([]System, len(list))
	copy(sortedSystems, list)
	sort.Slice(sortedSystems, func(i, j int) bool {
		return sortedSystems[i].SystemID < sortedSystems[j].SystemID
	})

	hash := sha256.New()
	for _, sys := range sortedSystems {
		if len(sys.Stargates) == 0 {
			continue
		}
		gates := make([]Stargate, len(sys.Stargates))
		copy(gates, sys.Stargates)
		sort.Slice(gates, func(i, j int) bool {
			if gates[i].ID == gates[j].ID {
				return gates[i].DestinationStargateID < gates[j].DestinationStargateID
			}
			return gates[i].ID < gates[j].ID
		})

		fmt.Fprintf(hash, "%d:", sys.SystemID)
		for _, gate := range gates {
			fmt.Fprintf(hash, "%d>%d,", gate.ID, gate.DestinationStargateID)
		}
		hash.Write([]byte(";"))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func shortFingerprint(fingerprint string) string {
	if len(fingerprint) > 12 {
		return fingerprint[:12]
	}
	return fingerprint
}

// writeGeneratedSystems atomically replaces the generated systems file served
// by /api/systems.json so readers never observe a partial write.
func writeGeneratedSystems(list []System) error {
	data, err := json.Marshal(list)
	if err != nil {
		return fmt.Errorf("marshal systems: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(generatedSystemsPath), 0755); err != nil { // #nosec G301 -- generated output directory
		return fmt.Errorf("create generated directory: %w", err)
	}

	tmpPath := generatedSystemsPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil { // #nosec G306 -- public generated data
		return fmt.Errorf("write temp systems file: %w", err)
	}
	if err := os.Rename(tmpPath, generatedSystemsPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace systems file: %w", err)
	}
	return nil
}
