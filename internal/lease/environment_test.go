//go:build integration

package lease

// testEnvironment is what these tests run as. It is deliberately not
// "production": the isolation test proves the other environment's runs are
// left alone, so the two names have to differ.
const testEnvironment = "test"
