//go:build !windows

package pi

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	runFakeAOIfAsked()
	os.Exit(m.Run())
}
