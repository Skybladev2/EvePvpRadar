package main

import (
	"bytes"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	zkillboardcache "evepvpsearch/zkillboard-cache"
)

func TestParseLawlessSystems(t *testing.T) {
	t.Parallel()
	body := []byte(`[
		{"campaignId":163,"insurgencies":[
			{"corruptionState":5,"solarSystem":{"id":30003832}},
			{"corruptionState":4,"solarSystem":{"id":30003829}},
			{"corruptionState":5,"solarSystem":{"id":0}}
		]},
		{"campaignId":164,"insurgencies":[
			{"corruptionState":0,"solarSystem":{"id":30002057}},
			{"corruptionState":5,"solarSystem":{"id":30002058}}
		]}
	]`)

	got, err := parseLawlessSystems(body)
	if err != nil {
		t.Fatalf("parseLawlessSystems returned error: %v", err)
	}
	if !got[30003832] || !got[30002058] {
		t.Fatalf("expected lawless systems 30003832 and 30002058, got %v", got)
	}
	if got[30003829] || got[30002057] {
		t.Fatalf("only corruptionState 5 must be lawless, got %v", got)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 lawless systems, got %d: %v", len(got), got)
	}
}

func TestRenderMarksLawlessSystem(t *testing.T) {
	lawlessSystemsMu.RLock()
	origLawless := lawlessSystems
	lawlessSystemsMu.RUnlock()
	lawlessSystemsMu.Lock()
	lawlessSystems = map[int]bool{30000142: true}
	lawlessSystemsMu.Unlock()
	defer func() {
		lawlessSystemsMu.Lock()
		lawlessSystems = origLawless
		lawlessSystemsMu.Unlock()
	}()

	systems := []SystemInRange{{
		SystemID:    30000142,
		Name:        "Jita",
		Dist:        1,
		Security:    0.9,
		RecentKills: []CachedKillmail{{KillmailID: 1, KillmailTime: time.Now().UTC().Format("2006-01-02T15:04:05Z")}},
		Weight:      1,
	}}

	html := renderHTMLTableWithNames(systems, "proximity", nil, nil, "")
	if !strings.Contains(html, "data-lawless='true'") {
		t.Fatalf("expected data-lawless marker on lawless system row, got: %s", html)
	}
	if !strings.Contains(html, "system-lawless-badge") {
		t.Fatalf("expected lawless badge in system cell, got: %s", html)
	}
}

// TestRenderLawlessEscalatesFilterBand verifies the checkbox that controls a lawless row: a lawless
// highsec system is filtered as lowsec, while a lawless lowsec system is filtered as nullsec. Nullsec
// is lawless by design, so it never carries the lawless marker. The displayed security value is
// unchanged.
func TestRenderLawlessEscalatesFilterBand(t *testing.T) {
	lawlessSystemsMu.RLock()
	origLawless := lawlessSystems
	lawlessSystemsMu.RUnlock()
	lawlessSystemsMu.Lock()
	lawlessSystems = map[int]bool{60000001: true, 60000002: true, 60000003: true}
	lawlessSystemsMu.Unlock()
	defer func() {
		lawlessSystemsMu.Lock()
		lawlessSystems = origLawless
		lawlessSystemsMu.Unlock()
	}()

	const nullsecID = 60000003
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	systems := []SystemInRange{
		{SystemID: 60000001, Name: "Lawless Highsec", Dist: 1, Security: 0.5, RecentKills: []CachedKillmail{{KillmailID: 1, KillmailTime: now}}, Weight: 1},
		{SystemID: 60000002, Name: "Lawless Lowsec", Dist: 1, Security: 0.3, RecentKills: []CachedKillmail{{KillmailID: 2, KillmailTime: now}}, Weight: 1},
		{SystemID: nullsecID, Name: "Nullsec", Dist: 1, Security: -0.2, RecentKills: []CachedKillmail{{KillmailID: 3, KillmailTime: now}}, Weight: 1},
	}
	html := renderHTMLTableWithNames(systems, "proximity", nil, nil, "")

	wantBnB := map[int]string{
		60000001:  "data-sec='lowsec'",
		60000002:  "data-sec='nullsec'",
		nullsecID: "data-sec='nullsec'",
	}
	for id, want := range wantBnB {
		marker := "system-" + strconv.Itoa(id) + "'"
		idx := strings.Index(html, marker)
		if idx < 0 {
			t.Fatalf("system %d not found in rendered table", id)
		}
		rowEnd := strings.Index(html[idx:], ">")
		if rowEnd < 0 {
			t.Fatalf("system %d row tag not terminated", id)
		}
		rowTag := html[idx : idx+rowEnd]
		if !strings.Contains(rowTag, want) {
			t.Fatalf("system %d row = %q, want %q", id, rowTag, want)
		}
		// Nullsec is lawless by design, so it must not carry the insurgency lawless marker.
		if id == nullsecID && strings.Contains(rowTag, "data-lawless") {
			t.Fatalf("nullsec system %d must not be marked lawless, row = %q", id, rowTag)
		}
	}
}

func TestStartInsurgencyRefresherMockSeedsLawless(t *testing.T) {
	origMock := mockData
	lawlessSystemsMu.RLock()
	origLawless := lawlessSystems
	lawlessSystemsMu.RUnlock()
	defer func() {
		mockData = origMock
		lawlessSystemsMu.Lock()
		lawlessSystems = origLawless
		lawlessSystemsMu.Unlock()
	}()

	mockData = true
	startInsurgencyRefresher()

	if !isLawlessSystem(30002813) {
		t.Fatal("expected Tama (30002813) to be seeded as lawless in mock mode")
	}
	if !isLawlessSystem(mockLawlessHighsecSystemID) {
		t.Fatalf("expected highsec system %d to be seeded as lawless in mock mode", mockLawlessHighsecSystemID)
	}
}

// TestIsValidKillmailAllowsLawlessHighsec guards the highsec-lawless path: a highsec system with an
// active insurgency is PvP space, so a stargate kill there must be accepted even though the system is
// not lowsec/nullsec/Pochven/Thera and has no station. Without the lawless set the same kill is
// filtered out, which is what "not filtered out early" protects against.
func TestIsValidKillmailAllowsLawlessHighsec(t *testing.T) {
	origSystems := systems
	lawlessSystemsMu.RLock()
	origLawless := lawlessSystems
	lawlessSystemsMu.RUnlock()
	defer func() {
		systems = origSystems
		lawlessSystemsMu.Lock()
		lawlessSystems = origLawless
		lawlessSystemsMu.Unlock()
	}()

	const (
		systemID   = 50100001
		stargateID = 77000001
	)
	systems = append([]System{{
		SystemID:   systemID,
		SystemName: "Mock-Highsec-Lawless-Test",
		Security:   0.5, // highsec
		Stargates: []Stargate{{
			ID:                    stargateID,
			Position:              [3]float64{1000, 0, 0},
			DestinationStargateID: 30000142,
		}},
	}}, systems...)

	pos := &struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
		Z float64 `json:"z"`
	}{X: 1000} // at the stargate
	kill := &zkillboardcache.CachedKillmail{
		KillmailID:    42,
		KillmailTime:  time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		SolarSystemID: systemID,
		Victim:        zkillboardcache.ESIVictim{Position: pos},
		ZKBInfo: zkillboardcache.ZKillboardKill{
			KillmailID: 42,
			ZKB:        zkillboardcache.ZKillboardKillInfo{LocationID: stargateID},
		},
	}

	lawlessSystemsMu.Lock()
	lawlessSystems = map[int]bool{}
	lawlessSystemsMu.Unlock()
	if valid, _ := isValidKillmail(kill); valid {
		t.Fatal("expected highsec kill away from a station to be invalid when the system is not lawless")
	}

	lawlessSystemsMu.Lock()
	lawlessSystems = map[int]bool{systemID: true}
	lawlessSystemsMu.Unlock()
	valid, isStation := isValidKillmail(kill)
	if !valid || isStation {
		t.Fatalf("expected lawless highsec kill to be valid and not a station kill, got valid=%v isStation=%v", valid, isStation)
	}
}

func TestInsurgencyRefreshFromHeaders(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"no header", "", insurgencyMinRefresh},
		{"max-age below minimum", "public,max-age=30", insurgencyMinRefresh},
		{"max-age", "public,max-age=150", 150 * time.Second},
		{"s-maxage wins when larger", "public,max-age=150,s-maxage=600", 600 * time.Second},
		{"max-age wins when larger", "public,max-age=900,s-maxage=600", 900 * time.Second},
		{"malformed", "public,max-age=abc", insurgencyMinRefresh},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.header != "" {
				h.Set("Cache-Control", tc.header)
			}
			if got := insurgencyRefreshFromHeaders(h); got != tc.want {
				t.Errorf("insurgencyRefreshFromHeaders(%q) = %v, want %v", tc.header, got, tc.want)
			}
		})
	}
}

// TestNullsecFilterLabelTracksAvailability verifies the checkbox label falls back to "Nullsec" while
// the insurgency feed is unavailable and mentions lawless systems once it has been fetched.
func TestNullsecFilterLabelTracksAvailability(t *testing.T) {
	origAvailable := lawlessDataAvailable.Load()
	defer lawlessDataAvailable.Store(origAvailable)

	lawlessDataAvailable.Store(false)
	if got := nullsecFilterLabel(); got != "Nullsec" {
		t.Fatalf("label with unavailable data = %q, want %q", got, "Nullsec")
	}

	lawlessDataAvailable.Store(true)
	if got := nullsecFilterLabel(); got != "Nullsec and lawless" {
		t.Fatalf("label with available data = %q, want %q", got, "Nullsec and lawless")
	}
}

// TestLowsecFilterLabelStaysPlainLowsec verifies the lowsec checkbox never advertises lawless
// coverage: lawless systems are surfaced by the nullsec checkbox, so the lowsec label stays "Lowsec"
// regardless of whether the insurgency feed is available.
func TestLowsecFilterLabelStaysPlainLowsec(t *testing.T) {
	origAvailable := lawlessDataAvailable.Load()
	defer lawlessDataAvailable.Store(origAvailable)

	for _, available := range []bool{false, true} {
		lawlessDataAvailable.Store(available)
		if got := lowsecFilterLabel(); got != "Lowsec" {
			t.Fatalf("label with available=%v = %q, want %q", available, got, "Lowsec")
		}
	}
}

// TestMarkLawlessUnavailableDegradesCleanly verifies that a failed insurgency fetch clears the
// lawless set (so systems are filtered as before the feature) and reverts the checkbox label, without
// leaving any lawless marker in the rendered table.
func TestMarkLawlessUnavailableDegradesCleanly(t *testing.T) {
	origAvailable := lawlessDataAvailable.Load()
	lawlessSystemsMu.RLock()
	origLawless := lawlessSystems
	lawlessSystemsMu.RUnlock()
	defer func() {
		lawlessDataAvailable.Store(origAvailable)
		lawlessSystemsMu.Lock()
		lawlessSystems = origLawless
		lawlessSystemsMu.Unlock()
	}()

	setLawlessSystems(map[int]bool{30000142: true})
	setLawlessAvailable(true)

	markLawlessUnavailable()

	if isLawlessSystem(30000142) {
		t.Fatal("expected lawless set to be cleared after failed fetch")
	}
	if got := nullsecFilterLabel(); got != "Nullsec" {
		t.Fatalf("label after failed fetch = %q, want %q", got, "Nullsec")
	}

	systems := []SystemInRange{{
		SystemID:    30000142,
		Name:        "Jita",
		Dist:        1,
		Security:    0.9,
		RecentKills: []CachedKillmail{{KillmailID: 1, KillmailTime: time.Now().UTC().Format("2006-01-02T15:04:05Z")}},
		Weight:      1,
	}}
	html := renderHTMLTableWithNames(systems, "proximity", nil, nil, "")
	if strings.Contains(html, "data-lawless") || strings.Contains(html, "system-lawless-badge") {
		t.Fatalf("expected no lawless markers after failed fetch, got: %s", html)
	}
}

// TestIndexTemplateRendersSecurityFilterLabels checks the index template consumes both label vars, so
// the nullsec checkbox can follow lawless-data availability while the lowsec checkbox stays "Lowsec".
func TestIndexTemplateRendersSecurityFilterLabels(t *testing.T) {
	tmpl := template.Must(template.New("index").Delims("[[", "]]").ParseFS(staticFS, "static/index.html"))

	for _, tc := range []struct{ nullsec, lowsec string }{
		{"Nullsec", "Lowsec"},
		{"Nullsec and lawless", "Lowsec"},
	} {
		var buf bytes.Buffer
		data := map[string]interface{}{"NullsecFilterLabel": tc.nullsec, "LowsecFilterLabel": tc.lowsec}
		if err := tmpl.ExecuteTemplate(&buf, "index.html", data); err != nil {
			t.Fatalf("executing index template: %v", err)
		}
		for _, want := range []string{tc.nullsec, tc.lowsec} {
			if !strings.Contains(buf.String(), ">"+want+"</span>") {
				t.Fatalf("expected checkbox label %q in rendered index, got: %s", want, buf.String())
			}
		}
	}
}
