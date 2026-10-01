package wallet

import "testing"

func TestReady(t *testing.T) {
	if !Ready() {
		t.Fatal("baseline not ready")
	}
}
