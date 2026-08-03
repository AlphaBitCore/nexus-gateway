package vendorbill

import (
	"math"
	"testing"
)

// nearUSD compares money at sub-micro-dollar tolerance. Dividing a decimal
// string by 100 is not exact in float64 (9034.37/100 = 90.34370000000001), so
// exact equality would fail on a correct result. The tolerance is many orders of
// magnitude tighter than the 100x error these tests exist to catch.
func nearUSD(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

// The cost_report `amount` field is documented as "lowest currency units (e.g.
// cents)" with the conversion spelled out ("123.45" in "USD" represents $1.23),
// while its worked example reads like dollars. This test PINS the settled
// interpretation: amount is CENTS, divided by 100 into USD.
//
// Settled 2026-07-20 against live org data: the same window totals 71,543.56
// under a dollars reading vs 715.44 under cents, against an operator-confirmed
// invoice of ~560 USD for the cycle. If someone later "corrects" this back to
// dollars, this test fails loudly — a wrong scale is a 100x money error and must
// never ship silently in either direction.
func TestAnthropicAmountScale_IsCentsNotDollars(t *testing.T) {
	if !anthropicAmountIsCents {
		t.Fatal("amount is cents per the live-verified 2026-07-20 reconciliation; flipping anthropicAmountIsCents back needs new live evidence + updating this test")
	}

	got, err := normalizeAmount("123.78912")
	if err != nil {
		t.Fatalf("normalizeAmount: %v", err)
	}
	// Cents interpretation: 1.2378912 USD. A result of 123.78912 means someone
	// switched back to dollars — a 100x overstatement of every vendor bill.
	if !nearUSD(got, 1.2378912) {
		t.Fatalf("amount scale drifted: got %v, want 1.2378912 (cents/100). A value of 123.78912 means someone switched to dollars.", got)
	}
}

// Guards the real-wire shape: the live API returns amount as a decimal STRING,
// and the observed 2026-07-15 value must land as USD 90.34, not 9034.37.
func TestAnthropicAmountScale_LiveObservedValue(t *testing.T) {
	got, err := normalizeAmount("9034.37")
	if err != nil {
		t.Fatalf("normalizeAmount: %v", err)
	}
	if !nearUSD(got, 90.3437) {
		t.Fatalf("live 2026-07-15 bucket: got %v USD, want 90.3437 USD", got)
	}
}

func TestAnthropicNormalizeAmount_RejectsGarbage(t *testing.T) {
	if _, err := normalizeAmount("not-a-number"); err == nil {
		t.Fatal("a non-numeric amount must error, not parse to 0")
	}
}
