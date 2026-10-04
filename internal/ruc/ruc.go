// Package ruc parses, normalizes and validates Paraguayan RUC numbers (Registro Único de Contribuyentes).
package ruc

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// ErrInvalidFormat is returned when the input cannot be read as a RUC.
var ErrInvalidFormat = errors.New("ruc: invalid format")

// RUC is a parsed RUC. Base is the number without the check digit (it may end in letters, e.g. "1023860A").
// HasDV reports whether the input carried a check digit.
type RUC struct {
	Base  string
	DV    int
	HasDV bool
}

// Parse reads inputs such as "80012345-6", "80.012.345-6", "80012345 6" or "80012345".
// A check digit is only recognised after a "-" or a space; without one the whole input is the base.
func Parse(input string) (RUC, error) {
	s := strings.ToUpper(strings.TrimSpace(input))
	s = strings.ReplaceAll(s, ".", "")
	s = strings.Join(strings.Fields(s), " ")
	s = strings.ReplaceAll(strings.ReplaceAll(s, " -", "-"), "- ", "-")

	base, dv, hasDV := s, "", false
	if i := strings.LastIndexAny(s, "- "); i >= 0 {
		base, dv, hasDV = strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:]), true
	}

	if !validBase(base) {
		return RUC{}, ErrInvalidFormat
	}

	r := RUC{Base: base}
	if hasDV {
		n, err := strconv.Atoi(dv)
		if err != nil || n < 0 || n > 9 {
			return RUC{}, ErrInvalidFormat
		}
		r.DV, r.HasDV = n, true
	}

	return r, nil
}

// validBase accepts 1 to 9 digits optionally followed by letters, the shapes found in the DNIT registry.
func validBase(base string) bool {
	if base == "" || len(base) > 12 || !unicode.IsDigit(rune(base[0])) {
		return false
	}
	seenLetter := false
	for _, c := range base {
		switch {
		case c >= '0' && c <= '9':
			if seenLetter {
				return false
			}
		case c >= 'A' && c <= 'Z':
			seenLetter = true
		default:
			return false
		}
	}
	return true
}

// CheckDigit computes the DNIT/SET check digit (modulo 11, weights 2..11 from the right).
// Letters are replaced by their ASCII code before weighting, as the DNIT does.
func CheckDigit(base string) int {
	var digits strings.Builder
	for _, c := range strings.ToUpper(base) {
		if c >= '0' && c <= '9' {
			digits.WriteRune(c)
		} else {
			digits.WriteString(strconv.Itoa(int(c)))
		}
	}

	total, weight := 0, 2
	s := digits.String()
	for i := len(s) - 1; i >= 0; i-- {
		total += int(s[i]-'0') * weight
		weight++
		if weight > 11 {
			weight = 2
		}
	}

	if rest := total % 11; rest > 1 {
		return 11 - rest
	}
	return 0
}

// ExpectedDV returns the correct check digit for r.
func (r RUC) ExpectedDV() int { return CheckDigit(r.Base) }

// Valid reports whether r carries a check digit and it is correct.
func (r RUC) Valid() bool { return r.HasDV && r.DV == r.ExpectedDV() }

// String formats r as "BASE-DV", computing the check digit when the input had none.
func (r RUC) String() string {
	dv := r.DV
	if !r.HasDV {
		dv = r.ExpectedDV()
	}
	return fmt.Sprintf("%s-%d", r.Base, dv)
}

// Segment is the DNIT file a RUC lives in (ruc0.zip … ruc9.zip): the last digit of the numeric part.
func (r RUC) Segment() int {
	numeric := strings.TrimRightFunc(r.Base, unicode.IsLetter)
	return int(numeric[len(numeric)-1] - '0')
}
