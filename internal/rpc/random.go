package rpc

import "crypto/rand"

// cryptoRead is a tiny package-private wrapper so submitjob.go's
// readRandom var can swap implementations in tests without touching
// crypto/rand directly.
func cryptoRead(b []byte) (int, error) {
	return rand.Read(b)
}
