package e2e

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
)

// testLogger is an interface for logging in tests
type testLogger interface {
	Logf(format string, args ...interface{})
	Log(args ...interface{})
}

// ginkgoLogger wraps GinkgoWriter to implement testLogger
type ginkgoLogger struct{}

func (g ginkgoLogger) Logf(format string, args ...interface{}) {
	fmt.Fprintf(GinkgoWriter, format+"\n", args...)
}

func (g ginkgoLogger) Log(args ...interface{}) {
	fmt.Fprintln(GinkgoWriter, args...)
}

var defaultLogger testLogger = ginkgoLogger{}
