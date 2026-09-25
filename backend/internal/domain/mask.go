package domain

import (
	"errors"
	"strings"
)

// Masks describe generated identifiers such as serial numbers and license
// plates (D41): 9 is a digit, A an uppercase letter, X an uppercase hex digit
// and a backslash escapes the next character. Anything else is literal.

// ValidateMask checks a mask.
func ValidateMask(mask string) error {
	if mask == "" {
		return errors.New("mask is empty")
	}
	if len(mask) > 64 {
		return errors.New("mask is longer than 64 characters")
	}
	if strings.HasSuffix(mask, `\`) && !strings.HasSuffix(mask, `\\`) {
		return errors.New("mask ends with a lone backslash")
	}
	variable := false
	escaped := false
	for _, r := range mask {
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
		case r == '9' || r == 'A' || r == 'X':
			variable = true
		}
	}
	if !variable {
		return errors.New("mask has no variable positions (9, A or X)")
	}
	return nil
}

// ExpandMask fills a mask. next returns a pseudo-random number in [0, n).
func ExpandMask(mask string, next func(n int) int) string {
	const letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	escaped := false
	for _, r := range mask {
		switch {
		case escaped:
			b.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case r == '9':
			b.WriteByte(byte('0' + next(10)))
		case r == 'A':
			b.WriteByte(letters[next(len(letters))])
		case r == 'X':
			b.WriteByte(hex[next(len(hex))])
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
