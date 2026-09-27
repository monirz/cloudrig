package cloudrig

import (
	"os"
	"slices"
	"strings"
	"testing"
)

var selfEnvKeys = []string{"PUBSUB_EMULATOR_HOST", "FIRESTORE_EMULATOR_HOST", "STORAGE_EMULATOR_HOST", "CLOUDRIG_ENDPOINT"}

// unsetEnv clears keys for one test; t.Setenv first so they are restored after.
func unsetEnv(t *testing.T, keys ...string) {
	for _, k := range keys {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestSelfEnvPointsFunctionsAtEmulator(t *testing.T) {
	unsetEnv(t, selfEnvKeys...)

	got := selfEnv("127.0.0.1:4599")
	want := []string{
		"PUBSUB_EMULATOR_HOST=127.0.0.1:4599",
		"FIRESTORE_EMULATOR_HOST=127.0.0.1:4599",
		"STORAGE_EMULATOR_HOST=http://127.0.0.1:4599",
		"CLOUDRIG_ENDPOINT=http://127.0.0.1:4599",
	}
	if !slices.Equal(got, want) {
		t.Errorf("selfEnv = %q, want %q", got, want)
	}
}

func TestSelfEnvKeepsExplicitValue(t *testing.T) {
	unsetEnv(t, selfEnvKeys...)
	t.Setenv("STORAGE_EMULATOR_HOST", "http://elsewhere:9000")

	for _, kv := range selfEnv("127.0.0.1:4599") {
		if strings.HasPrefix(kv, "STORAGE_EMULATOR_HOST=") {
			t.Errorf("selfEnv overrode an explicit STORAGE_EMULATOR_HOST with %q", kv)
		}
	}
}
