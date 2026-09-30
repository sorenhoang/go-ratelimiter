package leakybucket_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package if any test leaves a goroutine behind.
//
// This is the only package in the repo that starts goroutines of its own, so it
// is the only one that can leak them. A Queue whose Close forgot to stop its
// worker would otherwise pass every behavioural test in this file while leaking
// one goroutine and one ticker per queue ever created.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
