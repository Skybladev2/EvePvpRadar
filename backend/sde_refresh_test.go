package main

import "testing"

func TestStargateLayoutFingerprintDetectsChanges(t *testing.T) {
	base := []System{
		{SystemID: 1, Stargates: []Stargate{{ID: 11, DestinationStargateID: 2}}},
		{SystemID: 2, Stargates: []Stargate{{ID: 21, DestinationStargateID: 1}}},
	}

	sameDifferentOrder := []System{
		{SystemID: 2, Stargates: []Stargate{{ID: 21, DestinationStargateID: 1}}},
		{SystemID: 1, Stargates: []Stargate{{ID: 11, DestinationStargateID: 2}}},
	}
	if stargateLayoutFingerprint(base) != stargateLayoutFingerprint(sameDifferentOrder) {
		t.Fatal("fingerprint should ignore system and stargate order")
	}

	changedDestination := []System{
		{SystemID: 1, Stargates: []Stargate{{ID: 11, DestinationStargateID: 3}}},
		{SystemID: 2, Stargates: []Stargate{{ID: 21, DestinationStargateID: 1}}},
	}
	if stargateLayoutFingerprint(base) == stargateLayoutFingerprint(changedDestination) {
		t.Fatal("fingerprint should change when a stargate destination changes")
	}

	addedGate := []System{
		{SystemID: 1, Stargates: []Stargate{
			{ID: 11, DestinationStargateID: 2},
			{ID: 12, DestinationStargateID: 3},
		}},
		{SystemID: 2, Stargates: []Stargate{{ID: 21, DestinationStargateID: 1}}},
	}
	if stargateLayoutFingerprint(base) == stargateLayoutFingerprint(addedGate) {
		t.Fatal("fingerprint should change when a stargate is added")
	}
}
