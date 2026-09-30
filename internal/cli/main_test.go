package cli

import (
	"os"
	"testing"

	"github.com/lokalhub/kloo/internal/configtest"
)

// TestMain isolates the profile search for the whole package. Several tests here
// assert against config.DefaultModel / DefaultEndpoint, and the search chain reads
// $HOME — so without this they assert things about the developer's laptop and
// disagree with CI, with nothing in the output to say which one is lying.
func TestMain(m *testing.M) {
	restore := configtest.IsolateProfileSearchForPackage()
	code := m.Run()
	restore()
	os.Exit(code)
}
