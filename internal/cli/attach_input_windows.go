package cli

import (
	"errors"
	"os"
)

func discardPendingInput(*os.File) error {
	return errors.New("discarding buffered terminal input is unsupported on this platform")
}
