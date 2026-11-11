package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/shaul/mesh/internal/release"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: mesh-release-sign metadata|wrap|verify|verify-metadata|refresh ...")
	}
	switch args[0] {
	case "metadata":
		return validateMetadata(args[1:])
	case "wrap":
		return wrap(args[1:])
	case "verify":
		return verify(args[1:])
	case "verify-metadata":
		return verifyMetadata(args[1:])
	case "refresh":
		return refresh(args[1:])
	default:
		return fmt.Errorf("unknown release signing command %q", args[0])
	}
}

func validateMetadata(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: mesh-release-sign metadata FILE")
	}
	contents, err := readBounded(args[0], release.MaxMetadataBytes)
	if err != nil {
		return err
	}
	_, err = release.DecodeUpdateMetadata(contents)
	if err != nil {
		return fmt.Errorf("validate metadata: %w", err)
	}
	return nil
}

func wrap(args []string) error {
	if len(args) < 3 {
		return errors.New("usage: mesh-release-sign wrap BUNDLE OUTPUT SUBJECT...")
	}
	bundle, err := readBounded(args[0], release.MaxBundleBytes)
	if err != nil {
		return err
	}
	envelope, err := release.WrapBundle(bundle)
	if err != nil {
		return fmt.Errorf("wrap original bundle: %w", err)
	}
	if err := verifySubjects(envelope, args[2:]); err != nil {
		return err
	}
	if err := os.WriteFile(args[1], envelope, 0600); err != nil { //nolint:gosec // The publisher explicitly selects the output file.
		return fmt.Errorf("write signature envelope: %w", err)
	}
	return nil
}

func verify(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: mesh-release-sign verify ENVELOPE SUBJECT...")
	}
	envelope, err := readBounded(args[0], release.MaxSignatureBytes)
	if err != nil {
		return err
	}
	return verifySubjects(envelope, args[1:])
}

func verifySubjects(envelope []byte, subjects []string) error {
	trust, err := release.LoadTrustRoot(os.Getenv("MESH_RELEASE_TRUST_CACHE"))
	if err != nil {
		return fmt.Errorf("load release trust: %w", err)
	}
	for _, path := range subjects {
		if err := verifySubject(trust, envelope, path); err != nil {
			return err
		}
	}
	return nil
}

func verifySubject(trust release.TrustRoot, envelope []byte, path string) error {
	file, err := os.Open(path) //nolint:gosec // Subject paths are explicit publisher inputs.
	if err != nil {
		return fmt.Errorf("open subject %s: %w", path, err)
	}
	defer file.Close() //nolint:errcheck // The hash read result is authoritative.
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return fmt.Errorf("hash subject %s: %w", path, err)
	}
	if err := release.VerifyArtifactSignature(trust, hex.EncodeToString(hash.Sum(nil)), envelope); err != nil {
		return fmt.Errorf("verify subject %s: %w", path, err)
	}
	return nil
}

func verifyMetadata(args []string) error {
	if len(args) != 4 {
		return errors.New("usage: mesh-release-sign verify-metadata METADATA ENVELOPE MANIFEST PAGE")
	}
	var inputs [][]byte
	for index, path := range args {
		maximum := int64(release.MaxMetadataBytes)
		if index == 1 {
			maximum = release.MaxSignatureBytes
		}
		contents, err := readBounded(path, maximum)
		if err != nil {
			return err
		}
		inputs = append(inputs, contents)
	}
	trust, err := release.LoadTrustRoot(os.Getenv("MESH_RELEASE_TRUST_CACHE"))
	if err != nil {
		return fmt.Errorf("load release trust: %w", err)
	}
	_, err = release.VerifyMetadata(trust, inputs[0], inputs[1], inputs[2], inputs[3])
	if err != nil {
		return fmt.Errorf("verify release metadata: %w", err)
	}
	return nil
}

func refresh(args []string) error {
	if len(args) != 1 || args[0] == "" {
		return errors.New("usage: mesh-release-sign refresh CACHE")
	}
	_, err := release.RefreshTrust(context.Background(), args[0], release.TrustRefreshBudget{Documents: 8, Duration: 30 * time.Second})
	if err != nil {
		return fmt.Errorf("refresh release trust: %w", err)
	}
	return nil
}

func readBounded(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path) //nolint:gosec // The publisher explicitly selects local input files.
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close() //nolint:errcheck // The read result is authoritative.
	contents, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if int64(len(contents)) > maximum {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maximum)
	}
	return contents, nil
}
