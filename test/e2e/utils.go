package e2e

import (
	"os/exec"
)

// isKindAvailable checks if kind is available in PATH
func isKindAvailable() bool {
	_, err := exec.LookPath("kind")
	return err == nil
}

// ptrOf returns a pointer to the given value
func ptrOf[T any](t T) *T {
	return &t
}
