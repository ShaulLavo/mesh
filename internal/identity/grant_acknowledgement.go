package identity

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
)

type grantAcknowledgementWriter interface {
	Truncate(int64) error
	WriteAt([]byte, int64) (int, error)
	Sync() error
}

func grantsAcknowledged(lock io.ReaderAt, digest [sha256.Size]byte) (bool, error) {
	var receipt [sha256.Size + 1]byte
	n, err := lock.ReadAt(receipt[:], 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("identity: read grant acknowledgement: %w", err)
	}
	return n == sha256.Size && bytes.Equal(receipt[:n], digest[:]), nil
}

func invalidateGrantAcknowledgement(lock grantAcknowledgementWriter) error {
	if err := lock.Truncate(0); err != nil {
		return fmt.Errorf("identity: invalidate grant acknowledgement: %w", err)
	}
	if err := lock.Sync(); err != nil {
		return fmt.Errorf("identity: sync grant acknowledgement invalidation: %w", err)
	}
	return nil
}

func acknowledgeGrants(lock grantAcknowledgementWriter, digest [sha256.Size]byte) error {
	if err := lock.Truncate(0); err != nil {
		return fmt.Errorf("identity: reset grant acknowledgement: %w", err)
	}
	n, err := lock.WriteAt(digest[:], 0)
	if err != nil {
		return fmt.Errorf("identity: write grant acknowledgement: %w", err)
	}
	if n != len(digest) {
		return fmt.Errorf("identity: write grant acknowledgement: %w", io.ErrShortWrite)
	}
	return nil
}

func persistGrants(lock grantAcknowledgementWriter, digest [sha256.Size]byte, publish func() error) error {
	// Managed writers keep this lock stable and invalidate before every replacement.
	// A lost receipt is a miss; a retained receipt follows durable publication.
	if err := invalidateGrantAcknowledgement(lock); err != nil {
		return err
	}
	if err := publish(); err != nil {
		return err
	}
	return acknowledgeGrants(lock, digest)
}
