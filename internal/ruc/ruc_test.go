package ruc

import "testing"

// Real rows from the DNIT registry (ruc0.txt, 2026-10-01).
var registrySamples = []struct {
	base string
	dv   int
}{
	{"2038893", 4},
	{"1000000", 3},
	{"1000060", 7},
	{"80179140", 5},
	{"1000470", 0},
	{"1023860A", 1},
	{"1132310A", 2},
	{"1163450B", 9},
	{"161130A", 8},
	{"1828480A", 7},
}

func TestCheckDigitMatchesTheRegistry(t *testing.T) {
	for _, s := range registrySamples {
		if got := CheckDigit(s.base); got != s.dv {
			t.Errorf("CheckDigit(%q) = %d, want %d", s.base, got, s.dv)
		}
	}
}

func TestParseAcceptsCommonFormats(t *testing.T) {
	cases := map[string]RUC{
		"2038893-4":      {Base: "2038893", DV: 4, HasDV: true},
		"2.038.893-4":    {Base: "2038893", DV: 4, HasDV: true},
		" 2038893  4 ":   {Base: "2038893", DV: 4, HasDV: true},
		"2038893":        {Base: "2038893"},
		"1023860a-1":     {Base: "1023860A", DV: 1, HasDV: true},
		"80.179.140 - 5": {Base: "80179140", DV: 5, HasDV: true},
	}
	for input, want := range cases {
		got, err := Parse(input)
		if err != nil {
			t.Errorf("Parse(%q) error: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("Parse(%q) = %+v, want %+v", input, got, want)
		}
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, input := range []string{"", "-", "abc", "A123", "123-", "123-12", "12A3-1", "123/4", "2038893-x"} {
		if _, err := Parse(input); err == nil {
			t.Errorf("Parse(%q) should fail", input)
		}
	}
}

func TestValidChecksTheDigit(t *testing.T) {
	ok, _ := Parse("2038893-4")
	bad, _ := Parse("2038893-5")
	missing, _ := Parse("2038893")

	if !ok.Valid() {
		t.Error("2038893-4 should be valid")
	}
	if bad.Valid() || bad.ExpectedDV() != 4 {
		t.Errorf("2038893-5 should be invalid with expected 4, got %d", bad.ExpectedDV())
	}
	if missing.Valid() {
		t.Error("a RUC without DV is not Valid (nothing to check)")
	}
}

func TestStringAndSegment(t *testing.T) {
	r, _ := Parse("1023860A")
	if r.String() != "1023860A-1" {
		t.Errorf("String() = %q", r.String())
	}
	if r.Segment() != 0 {
		t.Errorf("Segment() = %d, want 0", r.Segment())
	}
	r, _ = Parse("80012347")
	if r.Segment() != 7 {
		t.Errorf("Segment() = %d, want 7", r.Segment())
	}
}
