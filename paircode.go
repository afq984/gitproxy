package main

import (
	"crypto/rand"
	"fmt"
)

// generatePairCode returns a short, human-readable pairing code in the format
// "ABC-1234" (3 uppercase letters, hyphen, 4 digits). Used to visually match
// a push on the git client side with the corresponding approval prompt.
func generatePairCode() string {
	var b [7]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	letters := [3]byte{
		'A' + b[0]%26,
		'A' + b[1]%26,
		'A' + b[2]%26,
	}
	digits := [4]byte{
		'0' + b[3]%10,
		'0' + b[4]%10,
		'0' + b[5]%10,
		'0' + b[6]%10,
	}
	return fmt.Sprintf("%s-%s", letters[:], digits[:])
}
