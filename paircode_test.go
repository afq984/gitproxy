package main

import (
	"regexp"
	"testing"
)

func TestGeneratePairCode(t *testing.T) {
	code := generatePairCode()
	matched, _ := regexp.MatchString(`^[A-Z]{3}-[0-9]{4}$`, code)
	if !matched {
		t.Errorf("generatePairCode() = %q, want format ABC-1234", code)
	}
}

func TestGeneratePairCodeVariation(t *testing.T) {
	a := generatePairCode()
	b := generatePairCode()
	if a == b {
		t.Errorf("two consecutive calls returned the same code %q", a)
	}
}
