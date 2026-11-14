package account

import "testing"

// The low cost is for test binaries only; a real install hashes at the
// production cost.
func TestPasswordCost(t *testing.T) {
	if got := costFor(false); got != productionPasswordCost {
		t.Errorf("outside a test, cost = %d, want %d", got, productionPasswordCost)
	}
	if productionPasswordCost < 12 {
		t.Errorf("production cost %d is below 12", productionPasswordCost)
	}
}
