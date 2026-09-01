package cloudrun

import "testing"

func TestMemoryBytes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"512Mi", 512 << 20},
		{"1Gi", 1 << 30},
		{"256Ki", 256 << 10},
		{"1G", 1_000_000_000},
		{"536870912", 536870912},
	}

	for _, c := range cases {
		got, err := memoryBytes(c.in)
		if err != nil {
			t.Errorf("memoryBytes(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("memoryBytes(%q) = %d, want %d", c.in, got, c.want)
		}
	}

	if _, err := memoryBytes("plenty"); err == nil {
		t.Error("a quantity that is not a number was accepted")
	}
}

// TestCPUNanos covers the milli-CPU suffix, which is the one that is easy to
// read as a plain number and be a thousand times wrong.
func TestCPUNanos(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"1", 1e9},
		{"2", 2e9},
		{"500m", 5e8},
		{"250m", 25e7},
	}

	for _, c := range cases {
		got, err := cpuNanos(c.in)
		if err != nil {
			t.Errorf("cpuNanos(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("cpuNanos(%q) = %d, want %d", c.in, got, c.want)
		}
	}

	if _, err := cpuNanos("lots"); err == nil {
		t.Error("a quantity that is not a number was accepted")
	}
}
