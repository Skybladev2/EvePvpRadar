package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// EVE's warzone insurgency API reports corruption/suppression progress for faction warfare
// insurgencies. Systems whose corruptionState reaches 5 become "lawless": PvP is enabled there
// regardless of the system's security band, so they belong in the radar and are filtered together
// with nullsec.
const (
	insurgencyURL = "https://www.eveonline.com/api/warzone/insurgency"

	// Never poll more often than once a minute, no matter what the cache headers say.
	insurgencyMinRefresh = time.Minute
	// Used when the response carries no usable Cache-Control max-age/s-maxage.
	insurgencyDefaultRefresh = time.Minute
	insurgencyHTTPTimeout    = 15 * time.Second
	// Response cap; the payload is ~9KB today, so this is generous.
	insurgencyMaxBody = 4 << 20

	// corruptionState value that makes a system lawless.
	insurgencyCorruptionLawless = 5
)

var lawlessSystemsGauge = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "evepvpsearch_lawless_systems",
	Help: "Current number of lawless systems (insurgency corruptionState=5)",
})

var (
	lawlessSystemsMu sync.RWMutex
	lawlessSystems   = map[int]bool{}
	// lawlessDataAvailable reports whether the most recent insurgency refresh succeeded. While it is
	// false the lawless set is empty, systems fall back to their normal security-band filtering, and
	// the security checkbox is rendered as "Nullsec" instead of "Nullsec and lawless".
	lawlessDataAvailable atomic.Bool
)

// isLawlessSystem reports whether the system currently has corruptionState 5.
func isLawlessSystem(systemID int) bool {
	lawlessSystemsMu.RLock()
	defer lawlessSystemsMu.RUnlock()
	return lawlessSystems[systemID]
}

// lawlessDataIsAvailable reports whether the lawless set reflects a successful insurgency fetch.
func lawlessDataIsAvailable() bool {
	return lawlessDataAvailable.Load()
}

// nullsecFilterLabel is the label of the security checkbox. Lawless systems are only mentioned while
// the insurgency feed is available; otherwise the filter behaves as a plain nullsec filter.
func nullsecFilterLabel() string {
	if lawlessDataIsAvailable() {
		return "Nullsec and lawless"
	}
	return "Nullsec"
}

// setLawlessAvailable records the outcome of an insurgency refresh. When the value changes the cached
// index HTML is invalidated so the security checkbox label follows suit.
func setLawlessAvailable(available bool) {
	if lawlessDataAvailable.Swap(available) == available {
		return
	}
	// killmailCache is nil during tests and before startup; skipping the invalidation keeps the
	// background table rebuild from running there.
	if killmailCache != nil {
		invalidateIndexHTMLCache()
	}
}

// markLawlessUnavailable handles a failed insurgency fetch: the lawless set is cleared so systems are
// filtered exactly as they were before the lawless feature (by security band), without surfacing any
// error, and the checkbox reverts to "Nullsec".
func markLawlessUnavailable() {
	setLawlessAvailable(false)
	setLawlessSystems(map[int]bool{})
}

// setLawlessSystems atomically replaces the lawless set. When it actually changed we run a full
// recalculation: a system that just became lawless may have had highsec stargate kills dropped by
// isValidKillmail, and one that stopped being lawless must move back to the highsec bucket. The
// recalculation also rebuilds the ready tables, so the badge appears/disappears immediately.
func setLawlessSystems(next map[int]bool) {
	lawlessSystemsMu.Lock()
	changed := len(lawlessSystems) != len(next)
	if !changed {
		for id := range next {
			if !lawlessSystems[id] {
				changed = true
				break
			}
		}
	}
	lawlessSystems = next
	lawlessSystemsMu.Unlock()
	lawlessSystemsGauge.Set(float64(len(next)))
	if changed && killmailCache != nil {
		EnsureRecalculated()
	}
}

// insurgencyCampaign mirrors the subset of the warzone insurgency payload we consume.
type insurgencyCampaign struct {
	Insurgencies []struct {
		CorruptionState int `json:"corruptionState"`
		SolarSystem     struct {
			ID int `json:"id"`
		} `json:"solarSystem"`
	} `json:"insurgencies"`
}

// parseLawlessSystems returns the set of system IDs whose corruptionState is lawless.
func parseLawlessSystems(body []byte) (map[int]bool, error) {
	var campaigns []insurgencyCampaign
	if err := json.Unmarshal(body, &campaigns); err != nil {
		return nil, err
	}
	lawless := make(map[int]bool)
	for _, c := range campaigns {
		for _, ins := range c.Insurgencies {
			if ins.CorruptionState == insurgencyCorruptionLawless && ins.SolarSystem.ID != 0 {
				lawless[ins.SolarSystem.ID] = true
			}
		}
	}
	return lawless, nil
}

// insurgencyRefreshFromHeaders derives how long to wait before the next request from the response's
// Cache-Control header. It honours max-age and s-maxage and never returns less than
// insurgencyMinRefresh, satisfying both the server's cache policy and our once-a-minute floor.
func insurgencyRefreshFromHeaders(h http.Header) time.Duration {
	refresh := insurgencyDefaultRefresh
	for _, directive := range strings.Split(h.Get("Cache-Control"), ",") {
		directive = strings.TrimSpace(strings.ToLower(directive))
		var value string
		switch {
		case strings.HasPrefix(directive, "s-maxage="):
			value = strings.TrimPrefix(directive, "s-maxage=")
		case strings.HasPrefix(directive, "max-age="):
			value = strings.TrimPrefix(directive, "max-age=")
		default:
			continue
		}
		seconds, err := strconv.Atoi(value)
		if err != nil || seconds <= 0 {
			continue
		}
		if d := time.Duration(seconds) * time.Second; d > refresh {
			refresh = d
		}
	}
	if refresh < insurgencyMinRefresh {
		refresh = insurgencyMinRefresh
	}
	return refresh
}

// refreshLawlessSystems fetches the insurgency feed once and returns the delay before the next
// fetch. When the endpoint is unavailable or its body cannot be parsed the lawless set is cleared,
// so systems are filtered as before the lawless feature and no error reaches the UI.
func refreshLawlessSystems() time.Duration {
	req, err := http.NewRequest(http.MethodGet, insurgencyURL, nil)
	if err != nil {
		log.Printf("insurgency: failed to build request: %v", err)
		markLawlessUnavailable()
		return insurgencyDefaultRefresh
	}
	setUserAgent(req)

	client := &http.Client{Timeout: insurgencyHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("insurgency: request failed: %v", err)
		markLawlessUnavailable()
		return insurgencyDefaultRefresh
	}
	defer resp.Body.Close()

	refresh := insurgencyRefreshFromHeaders(resp.Header)

	body, err := io.ReadAll(io.LimitReader(resp.Body, insurgencyMaxBody))
	if err != nil {
		log.Printf("insurgency: failed to read response: %v", err)
		markLawlessUnavailable()
		return refresh
	}
	if resp.StatusCode != http.StatusOK {
		log.Printf("insurgency: unexpected status %d", resp.StatusCode)
		markLawlessUnavailable()
		return refresh
	}

	lawless, err := parseLawlessSystems(body)
	if err != nil {
		log.Printf("insurgency: failed to decode response: %v", err)
		markLawlessUnavailable()
		return refresh
	}

	setLawlessSystems(lawless)
	setLawlessAvailable(true)
	log.Printf("insurgency: %d lawless systems (next refresh in %v)", len(lawless), refresh)
	return refresh
}

// startInsurgencyRefresher keeps the lawless-system set fresh in the background. In mock mode it
// seeds a lowsec system so the badge/filter can be exercised without network access.
func startInsurgencyRefresher() {
	if mockData {
		// Tama (lowsec) is already part of the mock kill set; marking it lawless shows the badge.
		log.Printf("insurgency: mock mode, marking Tama (%d) as lawless", 30002813)
		setLawlessSystems(map[int]bool{30002813: true})
		setLawlessAvailable(true)
		return
	}
	go func() {
		for {
			time.Sleep(refreshLawlessSystems())
		}
	}()
}
