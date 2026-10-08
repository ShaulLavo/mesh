package apppill

import (
	"os"
	"testing"

	"github.com/shaul/mesh/internal/testdomains"
)

func TestMain(m *testing.M) {
	cleanup := testdomains.Setup()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
