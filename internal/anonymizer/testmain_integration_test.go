//go:build integration

package anonymizer

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/joho/godotenv"
)

// TestMain loads .env from the project root so integration tests
// pick up ANONYMIZER_BASE_URL and ANONYMIZER_API_KEY automatically.
func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	_ = godotenv.Load(filepath.Join(root, ".env"))

	os.Exit(m.Run())
}
