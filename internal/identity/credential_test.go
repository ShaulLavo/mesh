package identity

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestCredentialNeverCreatesOnReadOrOverwritesDamagedKey(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "credential")
	if _, _, err := LoadCredential(directory); err == nil {
		t.Fatal("missing credential loaded")
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatal("read initialized credentials")
	}
	host, err := CreateCredential(directory)
	if err != nil {
		t.Fatal(err)
	}
	same, err := CreateCredential(directory)
	if err != nil || same.ID != host.ID {
		t.Fatal("keygen replaced key", err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	broken := []byte("malformed private key")
	if err := root.WriteFile(privateKeyName, broken, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateCredential(directory); err == nil {
		t.Fatal("malformed existing key regenerated")
	}
	got, err := root.ReadFile(privateKeyName)
	if err != nil || !bytes.Equal(got, broken) {
		t.Fatal("damaged key overwritten")
	}
}
func TestCredentialRejectsSymlinksHardlinksAndOversize(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "credential")
			if _, err := CreateCredential(directory); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(directory)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			err = damageCredential(root, kind)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := LoadCredential(directory); err == nil {
				t.Fatal("unsafe credential loaded")
			}
		})
	}
}
func TestConcurrentCredentialKeygenRetainsWinner(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "credential")
	var joined sync.WaitGroup
	results := make(chan Host, 8)
	failures := make(chan error, 8)
	for range 8 {
		joined.Go(func() { host, err := CreateCredential(directory); results <- host; failures <- err })
	}
	joined.Wait()
	close(results)
	close(failures)
	final, _, err := LoadCredential(directory)
	if err != nil {
		t.Fatal(err)
	}
	for host := range results {
		if host.ID != "" && host.ID != final.ID {
			t.Fatal("keygen replaced a concurrent winner")
		}
	}
	successes := 0
	for err := range failures {
		if err == nil {
			successes++
		}
	}
	if successes == 0 {
		t.Fatal("no keygen caller retained the winner")
	}
}

func damageCredential(root *os.Root, kind string) error {
	var err error
	switch kind {
	case "oversize":
		err = root.WriteFile(privateKeyName, make([]byte, (16<<10)+1), 0600)
	case "symlink", "hardlink":
		if err := root.Rename(privateKeyName, "original"); err != nil {
			return fmt.Errorf("fixture move credential: %w", err)
		}
		if kind == "symlink" {
			err = root.Symlink("original", privateKeyName)
		} else {
			err = root.Link("original", privateKeyName)
		}
	}
	if err != nil {
		return fmt.Errorf("fixture damage credential: %w", err)
	}
	return nil
}
