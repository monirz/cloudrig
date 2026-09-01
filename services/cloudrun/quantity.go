package cloudrun

import (
	"fmt"
	"strconv"
	"strings"
)

// Kubernetes quantities are what Cloud Run speaks, and Docker wants plain
// numbers: bytes for memory, billionths of a CPU for cpu. These convert.

// memoryBytes reads a memory quantity such as 512Mi, 1Gi or 536870912.
func memoryBytes(quantity string) (int64, error) {
	if quantity == "" {
		return 0, nil
	}

	// Binary suffixes are what gcloud sends; the decimal ones are legal too.
	suffixes := []struct {
		suffix string
		factor int64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
		{"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12},
	}
	for _, s := range suffixes {
		if digits, ok := strings.CutSuffix(quantity, s.suffix); ok {
			n, err := strconv.ParseFloat(digits, 64)
			if err != nil {
				return 0, fmt.Errorf("memory %q: %w", quantity, err)
			}
			return int64(n * float64(s.factor)), nil
		}
	}

	n, err := strconv.ParseFloat(quantity, 64)
	if err != nil {
		return 0, fmt.Errorf("memory %q is not a quantity: %w", quantity, err)
	}
	return int64(n), nil
}

// cpuNanos reads a CPU quantity such as 1, 2 or 500m, in billionths of a CPU.
func cpuNanos(quantity string) (int64, error) {
	if quantity == "" {
		return 0, nil
	}

	// An m suffix is milli-CPU: 500m is half a core.
	if digits, ok := strings.CutSuffix(quantity, "m"); ok {
		n, err := strconv.ParseFloat(digits, 64)
		if err != nil {
			return 0, fmt.Errorf("cpu %q: %w", quantity, err)
		}
		return int64(n * 1e6), nil
	}

	n, err := strconv.ParseFloat(quantity, 64)
	if err != nil {
		return 0, fmt.Errorf("cpu %q is not a quantity: %w", quantity, err)
	}
	return int64(n * 1e9), nil
}
