package repository

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Use smaller RSA keys in tests: RSA-4096 keygen dominates wall time.
	if os.Getenv("HAKOBIN_TEST_RSA_BITS") == "" {
		os.Setenv("HAKOBIN_TEST_RSA_BITS", "2048")
	}
	os.Exit(m.Run())
}
