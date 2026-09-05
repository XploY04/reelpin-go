//go:build integration

package enqueue

// testEnvironment is what these tests run as. It is deliberately not
// "production": every isolation test proves that the other environment's rows
// are invisible, so the two names have to differ.
const testEnvironment = "test"
