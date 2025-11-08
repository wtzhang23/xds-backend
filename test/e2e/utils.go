package e2e

import (
	"os/exec"
)

// isKindAvailable checks if kind is available in PATH
func isKindAvailable() bool {
	_, err := exec.LookPath("kind")
	return err == nil
}

// isHelmAvailable checks if helm is available in PATH
func isHelmAvailable() bool {
	_, err := exec.LookPath("helm")
	return err == nil
}

// isKubectlAvailable checks if kubectl is available in PATH
func isKubectlAvailable() bool {
	_, err := exec.LookPath("kubectl")
	return err == nil
}

// isDockerAvailable checks if docker is available in PATH
func isDockerAvailable() bool {
	_, err := exec.LookPath("docker")
	return err == nil
}

// contains checks if a string contains a substring
func contains(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	if len(s) < len(substr) {
		return false
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
