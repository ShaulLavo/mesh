package session

import "fmt"

const MaxInputBytes = 1 << 20
const MaxLineBytes = (7 << 19) + 1 // includes the newline delimiter

// InputReceipt certifies admission to the worker's buffer. Delivery to the
// process can stop at any byte if the process closes stdin or the host fails.
type InputReceipt struct {
	WriteKey string `json:"writeKey"`
	Offset   uint64 `json:"offset,string"`
	Length   int    `json:"length"`
	Closed   bool   `json:"closed"`
}

func ValidateWriteKey(key string) error {
	if len(key) < 1 || len(key) > 128 {
		return fmt.Errorf("execution: write key must contain 1 to 128 ASCII bytes")
	}
	for _, ch := range []byte(key) {
		if ch < 0x21 || ch > 0x7e {
			return fmt.Errorf("execution: invalid write key")
		}
	}
	return nil
}
