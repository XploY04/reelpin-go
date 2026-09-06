package main

import (
	"testing"

	"github.com/XploY04/reelpin-go/internal/spend"
)

func TestOptionalGateKeepsANilPointerOutOfTheInterface(t *testing.T) {
	if gate := optionalGate(nil); gate != nil {
		t.Fatalf("optionalGate(nil) = %#v, want a nil interface", gate)
	}

	configured := &spend.Gate{}
	if gate := optionalGate(configured); gate == nil {
		t.Fatal("optionalGate(configured) = nil")
	}
}
