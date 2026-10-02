package inbound

import (
	"crypto/rand"
	"io"
)

// testRNG returns the CSPRNG used to generate test keys.
func testRNG() io.Reader { return rand.Reader }
