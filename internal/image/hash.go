package image

import "crypto/sha256"

func sha256HexBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hexEncode(sum[:])
}
