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

	withoutNamesHTML := renderHTMLTableWithNames(systems, "proximity", nil, nil, "")
	if strings.Contains(withoutNamesHTML, "/character/90000001/losses/") {
		t.Fatalf("expected no pilot character link without resolved names")
	}

	withNamesHTML := renderHTMLTableWithNames(systems, "proximity", map[int]string{
		attackerID: "Test Pilot",
	}, nil, "")

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
	}, nil, "")

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
	}, "")

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

// A route can pass through both Zarzakh and Thera. When the trade hub is Zarzakh,
// the Zarzakh label is redundant, but the Thera label must still be shown: the ship
// size limit comes from the Thera wormhole, so omitting "Thera" hides route information.
// A Thera-hub route, by contrast, drops the Thera label as redundant.
func TestRenderRouteSuffixKeepsTheraForZarzakhHub(t *testing.T) {
	kill := CachedKillmail{
		KillmailID:   1,
		KillmailTime: time.Now().UTC().Add(-5 * time.Minute).Format("2006-01-02T15:04:05Z"),
	}
	rows := []SystemInRange{
		{
			SystemID:    30000142,
			Name:        "Via Thera from Zarzakh",
			Dist:        14,
			TradeHub:    "Zarzakh",
			ViaThera:    true,
			MaxShipSize: "Battlecruiser",
			RecentKills: []CachedKillmail{kill},
			Route: []EveScoutSystem{
				{SystemID: ZarzakhSystemID, SystemName: "Zarzakh"},
				{SystemID: TheraSystemID, SystemName: "Thera"},
				{SystemID: 30000142, SystemName: "Via Thera from Zarzakh"},
			},
		},
		{
			SystemID:    30000143,
			Name:        "From Thera",
			Dist:        8,
			TradeHub:    "Thera",
			ViaThera:    true,
			MaxShipSize: "Freighter",
			RecentKills: []CachedKillmail{kill},
			Route: []EveScoutSystem{
				{SystemID: TheraSystemID, SystemName: "Thera"},
				{SystemID: 30000143, SystemName: "From Thera"},
			},
		},
		{
			SystemID:    30000144,
			Name:        "Direct Zarzakh",
			Dist:        9,
			TradeHub:    "Zarzakh",
			RecentKills: []CachedKillmail{kill},
			Route: []EveScoutSystem{
				{SystemID: ZarzakhSystemID, SystemName: "Zarzakh"},
				{SystemID: 30000144, SystemName: "Direct Zarzakh"},
			},
		},
	}

	html := renderHTMLTableWithNames(rows, "near_trade_hubs", nil, nil, "")
	if !strings.Contains(html, "14 (Thera, max Battlecruiser)") {
		t.Fatalf("expected Thera label for Zarzakh-hub route via Thera, got: %s", html)
	}
	if !strings.Contains(html, "8 (max Freighter)") {
		t.Fatalf("expected Thera label to be suppressed for Thera-hub route, got: %s", html)
	}
	if strings.Contains(html, "8 (Thera") {
		t.Fatalf("did not expect redundant Thera label for Thera hub, got: %s", html)
	}
	if !strings.Contains(html, "9</span>") {
		t.Fatalf("expected no suffix for direct Zarzakh route, got: %s", html)
	}
	if strings.Contains(html, "(Zarzakh)") {
		t.Fatalf("did not expect a redundant Zarzakh label for the Zarzakh hub, got: %s", html)
	}
}
