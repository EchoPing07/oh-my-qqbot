package image

import "encoding/hex"

func hexEncode(data []byte) string { return hex.EncodeToString(data) }
