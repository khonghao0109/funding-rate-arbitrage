package main

import (
	"io"
	"log"
	"os"
	"testing"
)

// The request log is for an operator's terminal; in a test run it is noise
// around the failures that matter.
func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}
