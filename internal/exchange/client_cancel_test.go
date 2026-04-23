package exchange

import "testing"

func TestMachineCancelReason(t *testing.T) {
	got := machineCancelReason("loyal-flexibility", "stale_or_wrong")
	want := "bot.loyal-flexibility.stale_or_wrong"
	if got != want {
		t.Fatalf("machineCancelReason() = %q want %q", got, want)
	}
}

func TestIsProtectedOrderID(t *testing.T) {
	if !isProtectedOrderID("validation:apr:1", []string{"validation:", "test:"}) {
		t.Fatal("expected validation prefix to be protected")
	}
	if isProtectedOrderID("mm:USDCcNGN-APR30-2026:buy:1", []string{"validation:", "test:"}) {
		t.Fatal("unexpected protection for normal mm order id")
	}
}
