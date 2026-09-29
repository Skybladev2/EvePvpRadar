package main

import (
	"bytes"
	"html/template"
	"net/http"
	"strings"
	"testing"
	"time"
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

// TestIndexTemplateRendersNullsecFilterLabel checks the index template consumes NullsecFilterLabel so
// the checkbox text follows the current lawless-data availability.
func TestIndexTemplateRendersNullsecFilterLabel(t *testing.T) {
	tmpl := template.Must(template.New("index").Delims("[[", "]]").ParseFS(staticFS, "static/index.html"))

	for _, want := range []string{"Nullsec", "Nullsec and lawless"} {
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, "index.html", map[string]interface{}{"NullsecFilterLabel": want}); err != nil {
			t.Fatalf("executing index template: %v", err)
		}
		if !strings.Contains(buf.String(), ">"+want+"</span>") {
			t.Fatalf("expected checkbox label %q in rendered index, got: %s", want, buf.String())
		}
	}
}
