package apps

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func excluded(name string) bool {
	for _, p := range strings.Split(filepath.ToSlash(name), "/") {
		if p == ".git" || p == "node_modules" || p == ".env" || strings.HasPrefix(p, ".env.") || p == ".ssh" || p == ".aws" || p == ".venv" || p == ".cache" || p == "__pycache__" {
			return true
		}
	}
	return false
}
func secretName(name string) bool {
	lower := strings.ToLower(filepath.Base(name))
	return strings.HasSuffix(lower, ".pem") || strings.HasSuffix(lower, ".key") || lower == "id_rsa" || lower == "id_ed25519" || lower == "credentials" || lower == "credentials.json" || lower == ".npmrc" || lower == ".netrc" || lower == ".pypirc" || lower == ".git-credentials"
}

// Pack copies source, excluding environment files, credentials and generated dependencies.
func Pack(ctx context.Context, dir string, w io.Writer) (string, error) {
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", err
	}
	home, _ := os.UserHomeDir()
	home, _ = filepath.EvalSymlinks(home)
	if canonical == home || filepath.Dir(canonical) == canonical {
		return "", errors.New("app: select an app directory instead of home or filesystem root")
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	h := sha256.New()
	gz := gzip.NewWriter(io.MultiWriter(w, h))
	tw := tar.NewWriter(gz)
	var total int64
	count := 0
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if excluded(name) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if secretName(name) {
			return fmt.Errorf("app: credential file %s; remove it from source", name)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("app: unsupported source file %s", name)
		}
		total += info.Size()
		count++
		if total > MaxArchive || count > 10000 {
			return errors.New("app: source exceeds 64MiB or 10000 files")
		}
		if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(name), Mode: int64(info.Mode().Perm()), Size: info.Size()}); err != nil {
			return err
		}
		f, err := root.Open(name)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		closeErr := f.Close()
		return errors.Join(copyErr, closeErr)
	})
	if err != nil {
		_ = tw.Close()
		_ = gz.Close()
		return "", err
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func unpack(archive, dir, digest string) error {
	f, err := os.Open(archive) //nolint:gosec // Archive is the origin-owned upload path, never a visitor-supplied filename.
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() > MaxArchive {
		return errors.New("app: compressed archive exceeds limit")
	}
	h := sha256.New()
	if _, err = io.Copy(h, io.LimitReader(f, MaxArchive+1)); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != digest {
		return errors.New("app: archive digest mismatch")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tr := tar.NewReader(gz)
	var total int64
	count := 0
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		clean := path.Clean(header.Name)
		if clean != header.Name || clean == "." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) || strings.Contains(clean, "\\") || excluded(clean) || secretName(clean) {
			return errors.New("app: unsafe archive path")
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != byte(0) {
			return errors.New("app: archive links and special files are forbidden")
		}
		total += header.Size
		count++
		if header.Size < 0 || total > MaxArchive || count > 10000 {
			return errors.New("app: expanded archive exceeds limits")
		}
		if err := root.MkdirAll(path.Dir(clean), 0700); err != nil {
			return err
		}
		out, err := root.OpenFile(clean, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600|fs.FileMode(header.Mode&0100))
		if err != nil {
			return err
		}
		_, err = io.CopyN(out, tr, header.Size)
		if closeErr := out.Close(); err != nil || closeErr != nil {
			return errors.Join(err, closeErr)
		}
	}
}
