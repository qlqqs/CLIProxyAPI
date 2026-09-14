// Package billing contains the deterministic member billing calculations.
package billing

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
)

const NanoUSDPerUSD int64 = 1_000_000_000

var ErrInvalidAmount = errors.New("billing: invalid USD amount")

// ParseNanoUSD parses a non-negative decimal USD amount with at most nine
// fractional digits. It never uses floating-point arithmetic.
func ParseNanoUSD(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") || strings.ContainsAny(value, "eE$") {
		return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, value)
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" {
		return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, value)
	}
	whole, frac := parts[0], ""
	if len(parts) == 2 {
		frac = parts[1]
	}
	if len(frac) > 9 {
		return 0, fmt.Errorf("%w: more than nine fractional digits", ErrInvalidAmount)
	}
	for _, r := range whole + frac {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, value)
		}
	}
	frac += strings.Repeat("0", 9-len(frac))
	n := new(big.Int)
	if _, ok := n.SetString(whole+frac, 10); !ok || !n.IsInt64() {
		return 0, fmt.Errorf("%w: overflow", ErrInvalidAmount)
	}
	return n.Int64(), nil
}

// FormatUSD renders nano-USD without losing non-zero values.
func FormatUSD(nano int64) string {
	if nano < 0 {
		return "-$" + FormatUSD(-nano)
	}
	whole := nano / NanoUSDPerUSD
	fraction := nano % NanoUSDPerUSD
	if fraction == 0 {
		return fmt.Sprintf("$%d", whole)
	}
	frac := fmt.Sprintf("%09d", fraction)
	frac = strings.TrimRight(frac, "0")
	return fmt.Sprintf("$%d.%s", whole, frac)
}

// FormatDecimal is FormatUSD without the currency prefix, suitable for API input fields.
func FormatDecimal(nano int64) string { out := FormatUSD(nano); return strings.TrimPrefix(out, "$") }
