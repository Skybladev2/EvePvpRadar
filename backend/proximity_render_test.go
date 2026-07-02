package main

import (
	"strings"
	"testing"
	"time"
)

func TestRenderProximityTableHidesStandingPilotIconWhenNamesResolved(t *testing.T) {
	origTypes := types
	origTypeIDToGroupName := typeIDToGroupName
	defer func() {
		types = origTypes
		typeIDToGroupName = origTypeIDToGroupName
	}()

	types = map[int]string{
		111: "Rifter",
		222: "200mm AutoCannon I",
		333: "Merlin",
	}
	typeIDToGroupName = map[int]string{
		111: "Frigate",
		333: "Frigate",
	}

	const attackerID = 90000001
	kill := CachedKillmail{
		KillmailID:   123456789,
		KillmailTime: time.Now().UTC().Add(-5 * time.Minute).Format("2006-01-02T15:04:05Z"),
		Victim: ESIVictim{
			ShipTypeID: 333,
		},
		Attackers: []ESIAttacker{
			{
				CharacterID:  attackerID,
				ShipTypeID:   111,
				WeaponTypeID: 222,
			},
		},
	}

	systems := []SystemInRange{
		{
			SystemID:    30000142,
			Name:        "Jita",
			Dist:        1,
			Security:    0.9,
			RecentKills: []CachedKillmail{kill},
			Weight:      1,
		},
	}

	withoutNamesHTML := renderHTMLTableWithNames(systems, "proximity", nil, nil)
	if strings.Contains(withoutNamesHTML, "/character/90000001/losses/") {
		t.Fatalf("expected no pilot character link without resolved names")
	}

	withNamesHTML := renderHTMLTableWithNames(systems, "proximity", map[int]string{
		attackerID: "Test Pilot",
	}, nil)

	if !strings.Contains(withNamesHTML, "zkillboard.com/asearch/#") {
		t.Fatalf("expected pilot zKill asearch losses link when names are resolved")
	}
	if !strings.Contains(withNamesHTML, "90000001") {
		t.Fatalf("expected pilot character id in zKill asearch hash")
	}
	if !strings.Contains(withNamesHTML, "%22shipID%22") || !strings.Contains(withNamesHTML, "%22id%22:111") {
		t.Fatalf("expected attacker ship type in zKill asearch hash")
	}
	if strings.Contains(withNamesHTML, "pilot-icon") {
		t.Fatalf("expected standing pilot icon markup to be hidden when pilot is only in one system")
	}
	if !strings.Contains(withNamesHTML, "aria-label='Test Pilot'") {
		t.Fatalf("expected resolved pilot name in accessibility label")
	}
}

func TestRenderProximityTableShowsRunningPilotIconWhenNamesResolvedInMultiSystem(t *testing.T) {
	origTypes := types
	origTypeIDToGroupName := typeIDToGroupName
	defer func() {
		types = origTypes
		typeIDToGroupName = origTypeIDToGroupName
	}()

	types = map[int]string{
		111: "Rifter",
		222: "200mm AutoCannon I",
		333: "Merlin",
	}
	typeIDToGroupName = map[int]string{
		111: "Frigate",
		333: "Frigate",
	}

	const attackerID = 90000001
	kill := CachedKillmail{
		KillmailID:   123456789,
		KillmailTime: time.Now().UTC().Add(-5 * time.Minute).Format("2006-01-02T15:04:05Z"),
		Victim: ESIVictim{
			ShipTypeID: 333,
		},
		Attackers: []ESIAttacker{
			{
				CharacterID:  attackerID,
				ShipTypeID:   111,
				WeaponTypeID: 222,
			},
		},
	}

	systems := []SystemInRange{
		{
			SystemID:    30000142,
			Name:        "Jita",
			Dist:        1,
			Security:    0.9,
			RecentKills: []CachedKillmail{kill},
			Weight:      1,
		},
		{
			SystemID:    30000143,
			Name:        "Amarr",
			Dist:        2,
			Security:    0.9,
			RecentKills: []CachedKillmail{kill},
			Weight:      1,
		},
	}

	withNamesHTML := renderHTMLTableWithNames(systems, "proximity", map[int]string{
		attackerID: "Test Pilot",
	}, nil)

	if !strings.Contains(withNamesHTML, "zkillboard.com/asearch/#") {
		t.Fatalf("expected pilot zKill asearch losses link when names are resolved")
	}
	if !strings.Contains(withNamesHTML, "90000001") {
		t.Fatalf("expected pilot character id in zKill asearch hash")
	}
	if !strings.Contains(withNamesHTML, "pilot-icon") {
		t.Fatalf("expected running pilot icon markup in proximity table when pilot appears in multiple systems")
	}
	if !strings.Contains(withNamesHTML, "aria-label='Test Pilot'") {
		t.Fatalf("expected resolved pilot name in accessibility label")
	}
}

func TestRenderProximityTableShowsESIFailurePilotTooltip(t *testing.T) {
	origTypes := types
	origTypeIDToGroupName := typeIDToGroupName
	defer func() {
		types = origTypes
		typeIDToGroupName = origTypeIDToGroupName
	}()

	types = map[int]string{
		111: "Rifter",
		222: "200mm AutoCannon I",
		333: "Merlin",
	}
	typeIDToGroupName = map[int]string{
		111: "Frigate",
		333: "Frigate",
	}

	const attackerID = 90000002
	kill := CachedKillmail{
		KillmailID:   123456790,
		KillmailTime: time.Now().UTC().Add(-5 * time.Minute).Format("2006-01-02T15:04:05Z"),
		Victim: ESIVictim{
			ShipTypeID: 333,
		},
		Attackers: []ESIAttacker{
			{
				CharacterID:  attackerID,
				ShipTypeID:   111,
				WeaponTypeID: 222,
			},
		},
	}

	systems := []SystemInRange{
		{
			SystemID:    30000142,
			Name:        "Jita",
			Dist:        1,
			Security:    0.9,
			RecentKills: []CachedKillmail{kill},
			Weight:      1,
		},
	}

	esiErr := esiCharacterNameFailureMsg(attackerID, "HTTP 404")
	html := renderHTMLTableWithNames(systems, "proximity", map[int]string{
		attackerID: "",
	}, map[int]string{
		attackerID: esiErr,
	})

	if !strings.Contains(html, "data-pilot-unresolved='true'") {
		t.Fatalf("expected unresolved pilot marker in HTML")
	}
	if !strings.Contains(html, "data-tooltip='"+esiErr+"'") {
		t.Fatalf("expected ESI failure tooltip in HTML, got: %s", html)
	}
	if strings.Contains(html, "data-tooltip='Pilot'") {
		t.Fatalf("expected detailed tooltip instead of generic Pilot")
	}
}
