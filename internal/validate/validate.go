// Package validate holds shared validators for the Go services.
package validate

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// decimal128 avoids importing the driver here; matched structurally.
type decimal128 = primitive.Decimal128

// json_Number matches encoding/json Number without importing it as a value.
type json_Number = json.Number

// StoreError is the domain error: servers catch it and return {ok: False, error}.
type StoreError struct{ Msg string }

func (e *StoreError) Error() string { return e.Msg }

func fail(format string, args ...any) *StoreError {
	return &StoreError{Msg: fmt.Sprintf(format, args...)}
}

var dateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// MacroKeys is the 5-macro set shared by health-check items.
var MacroKeys = []string{"kcal", "protein", "carbs", "fat", "fiber"}

// UTCNow returns timezone-aware timestamps so createdAt/updatedAt compare correctly.
func UTCNow() time.Time { return time.Now().UTC() }

// CheckDay rejects bad format AND non-calendar dates (e.g. 2026-02-30).
func CheckDay(day string) (string, error) {
	day = strings.TrimSpace(day)
	if !dateRE.MatchString(day) {
		return "", fail("bad date '%s' — use YYYY-MM-DD", day)
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return "", fail("bad date '%s' — not a real calendar date", day)
	}
	return day, nil
}

// CheckMacros requires all 5 macros, numeric, >= 0; names the missing field.
func CheckMacros(d map[string]any, ctx string) error {
	for _, k := range MacroKeys {
		v, ok := d[k]
		if !ok {
			return fail("%s missing '%s' — ask the user for it", ctx, k)
		}
		f, err := ToFloat(v)
		if err != nil {
			return fail("%s field '%s' must be a number", ctx, k)
		}
		if f < 0 {
			return fail("%s field '%s' must be >= 0", ctx, k)
		}
	}
	return nil
}

// SumTotals aggregates item macros to 1-decimal totals.
func SumTotals(items []map[string]any) map[string]float64 {
	out := map[string]float64{}
	for _, k := range MacroKeys {
		var s float64
		for _, it := range items {
			f, _ := ToFloat(it[k])
			s += f
		}
		out[k] = Round1(s)
	}
	return out
}

// ToFloat accepts float/int/Decimal128/numeric strings (Python float()
// parity for MCP clients sending string numbers).
func ToFloat(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case float32:
		return float64(n), nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case int32:
		return float64(n), nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0, errors.New("not a number")
		}
		return f, nil
	case json_Number:
		f, err := n.Float64()
		if err != nil {
			return 0, errors.New("not a number")
		}
		return f, nil
	case decimal128:
		f, err := strconv.ParseFloat(n.String(), 64)
		if err != nil {
			return 0, errors.New("not a number")
		}
		return f, nil
	default:
		return 0, errors.New("not a number")
	}
}

func Round1(f float64) float64 {
	if f >= 0 {
		return float64(int(f*10+0.5)) / 10
	}
	return float64(int(f*10-0.5)) / 10
}

