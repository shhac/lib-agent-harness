package session

import (
	"strconv"
	"strings"
)

// Amounts stay exact decimal strings from the wire to the caller. A float
// would round a currency value, and a balance is the one figure a caller may
// compare against a price.

// decimal accepts a plain, optionally negative decimal such as "0" or "12.50".
func decimal(s string) bool {
	s = strings.TrimPrefix(s, "-")
	whole, fraction, dotted := strings.Cut(s, ".")
	return len(s) <= 64 && digits(whole) && (!dotted || digits(fraction))
}

func digits(s string) bool {
	return s != "" && strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

// minorUnits writes an integer count of minor units as a decimal: 251 at
// exponent 2 is "2.51".
func minorUnits(minor string, exponent int) (string, bool) {
	n, err := strconv.ParseInt(minor, 10, 64)
	if err != nil || exponent < 0 || exponent > 18 {
		return "", false
	}
	sign := ""
	if n < 0 {
		sign = "-"
	}
	units := strings.TrimPrefix(strconv.FormatInt(n, 10), "-")
	if exponent == 0 {
		return sign + units, true
	}
	if len(units) <= exponent {
		units = strings.Repeat("0", exponent-len(units)+1) + units
	}
	split := len(units) - exponent
	return sign + units[:split] + "." + units[split:], true
}

func currencyCode(s string) bool {
	return len(s) == 3 && strings.IndexFunc(s, func(r rune) bool { return r < 'A' || r > 'Z' }) < 0
}

// providerCode keeps a provider's enumerated code, such as "out_of_credits",
// and drops anything that is not shaped like one, so free text never passes.
func providerCode(s *string) string {
	if s == nil || len(*s) == 0 || len(*s) > 64 {
		return ""
	}
	for i, r := range *s {
		if (r < 'a' || r > 'z') && (i == 0 || (r != '_' && (r < '0' || r > '9'))) {
			return ""
		}
	}
	return *s
}
