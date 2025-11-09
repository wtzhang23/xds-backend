package e2e

import (
	"os/exec"
)

// isKindAvailable checks if kind is available in PATH
func isKindAvailable() bool {
	_, err := exec.LookPath("kind")
	return err == nil
}
