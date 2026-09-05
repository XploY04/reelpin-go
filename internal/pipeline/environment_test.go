//go:build integration

package pipeline

// testEnvironment is what these tests run as. It is deliberately not
// "production": the isolation test proves the other environment's run is
// refused, so the two names have to differ.
const testEnvironment = "test"
