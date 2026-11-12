package cli

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"syscall"
	"time"
)

const replyRecordDirectory = "last-reply"
const replyRecordLimit = 1024

type replyRecord struct {
	MeshIdentity string    `json:"meshIdentity"`
	VerifiedAt   time.Time `json:"verifiedAt"`
}

func replyRecordName(identity string) string {
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:]) + ".json"
}

func openReplyDirectory(state string, create bool) (*os.Root, error) {
	root, err := os.OpenRoot(state)
	if err != nil {
		return nil, fmt.Errorf("open reply state: %w", err)
	}
	defer func() { _ = root.Close() }()
	if create {
		if err := root.Mkdir(replyRecordDirectory, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("create reply directory: %w", err)
		}
	}
	info, err := root.Lstat(replyRecordDirectory)
	if err != nil {
		return nil, fmt.Errorf("inspect reply directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, errors.New("reply directory must be private and regular")
	}
	directory, err := root.OpenRoot(replyRecordDirectory)
	if err != nil {
		return nil, fmt.Errorf("open reply directory: %w", err)
	}
	return directory, nil
}

func readReplyRecord(state, identity string) (time.Time, bool) {
	root, err := openReplyDirectory(state, false)
	if err != nil {
		return time.Time{}, false
	}
	defer func() { _ = root.Close() }()
	name := replyRecordName(identity)
	before, err := root.Lstat(name)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm() != 0o600 || before.Size() > replyRecordLimit {
		return time.Time{}, false
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return time.Time{}, false
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !os.SameFile(before, info) || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > replyRecordLimit {
		return time.Time{}, false
	}
	var value replyRecord
	decoder := json.NewDecoder(io.LimitReader(file, replyRecordLimit+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil || value.MeshIdentity != identity || value.VerifiedAt.IsZero() {
		return time.Time{}, false
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return time.Time{}, false
	}
	return value.VerifiedAt, true
}

func writeReplyRecord(state, identity string, at time.Time) error {
	if at.IsZero() || identity == "" {
		return errors.New("reply observation requires identity and time")
	}
	if prior, ok := readReplyRecord(state, identity); ok && at.Sub(prior) < time.Minute && prior.Sub(at) < time.Minute {
		return nil
	}
	data, err := json.Marshal(replyRecord{MeshIdentity: identity, VerifiedAt: at.UTC().Truncate(time.Millisecond)})
	if err != nil {
		return fmt.Errorf("encode reply record: %w", err)
	}
	if len(data) > replyRecordLimit {
		return errors.New("reply record exceeds its byte limit")
	}
	root, err := openReplyDirectory(state, true)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	name := ".reply-" + rand.Text()
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create reply record: %w", err)
	}
	defer func() { _ = root.Remove(name) }()
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("write reply record: %w", err)
	}
	if err := root.Rename(name, replyRecordName(identity)); err != nil {
		return fmt.Errorf("publish reply record: %w", err)
	}
	return nil
}

func (c *SQLiteCatalogCache) recordReply(host HostRecord, at time.Time) {
	if c.stateDir == "" || host.MeshIdentity == "" {
		return
	}
	if err := writeReplyRecord(c.stateDir, host.MeshIdentity, at); err != nil {
		log.Printf("catalog cache: skipped last-reply record for host %q: %v", host.ID, err)
	}
}
