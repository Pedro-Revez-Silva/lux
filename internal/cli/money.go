package cli

import (
	"math/big"
	"strings"

	"github.com/marcioapm/lux/internal/server"
)

// noValue stands for an amount lux does not have; never "0".
const noValue = "—"

// moneyDecimals is how many fractional digits amounts are shown with.
const moneyDecimals = 4

// money formats an API amount (a decimal string) with its currency code:
// rounded half-even to moneyDecimals, trailing zeros trimmed, never through
// a float. A non-zero amount that rounds to zero is shown as a bound
// ("<0.0001 USD"), so a real cost never reads as 0. An empty amount is
// noValue; a string that is not a decimal is shown as it came.
func money(amount, currency string) string {
	if amount == "" {
		return noValue
	}
	s, ok := roundDecimal(amount, moneyDecimals)
	if !ok {
		s = amount
	}
	if currency == "" {
		return s
	}
	return s + " " + currency
}

// roundDecimal rounds a plain decimal string ("-12.345") half-even to
// places fractional digits, then trims trailing zeros.
func roundDecimal(v string, places int) (string, bool) {
	neg := strings.HasPrefix(v, "-")
	digits := strings.TrimPrefix(strings.TrimPrefix(v, "-"), "+")
	whole, frac, _ := strings.Cut(digits, ".")
	if whole == "" && frac == "" || strings.Trim(whole+frac, "0123456789") != "" {
		return "", false
	}
	n, ok := new(big.Int).SetString(cmpOr(whole+frac, "0"), 10)
	if !ok {
		return "", false
	}
	nonZero := n.Sign() != 0
	if drop := len(frac) - places; drop > 0 {
		div := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(drop)), nil)
		q, r := new(big.Int).QuoRem(n, div, new(big.Int))
		// Half-even: above half rounds up; exactly half rounds to the even neighbour.
		switch c := new(big.Int).Lsh(r, 1).Cmp(div); {
		case c > 0, c == 0 && q.Bit(0) == 1:
			q.Add(q, big.NewInt(1))
		}
		n = q
	} else {
		places = len(frac)
	}
	if n.Sign() == 0 && nonZero {
		bound := "0." + strings.Repeat("0", places-1) + "1"
		if neg {
			return ">-" + bound, true
		}
		return "<" + bound, true
	}
	s := n.String()
	if len(s) <= places {
		s = strings.Repeat("0", places-len(s)+1) + s
	}
	out := s
	if places > 0 {
		out = strings.TrimRight(strings.TrimRight(s[:len(s)-places]+"."+s[len(s)-places:], "0"), ".")
	}
	if neg && n.Sign() != 0 {
		out = "-" + out
	}
	return out, true
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// isZero: the API's "0" (and its trimmed variants) for a final or estimate part.
func isZero(amount string) bool {
	return strings.Trim(strings.TrimPrefix(amount, "-"), "0.") == "" && amount != ""
}

// runCostCell is the Runs list's COST column: the total when the Run has
// one currency, "multi" for several, noValue while nothing is reported.
// A leading "~" marks an amount that may still change: part of it is an
// estimate, or a source has not answered (status incomplete).
func runCostCell(c *server.RunCostBrief) string {
	if c == nil || len(c.Totals) == 0 {
		return noValue
	}
	if len(c.Totals) > 1 {
		return "multi"
	}
	t := c.Totals[0]
	s := money(t.Amount, t.Currency)
	if !isZero(t.Estimate) || c.Status == "incomplete" {
		s = "~" + s
	}
	return s
}
