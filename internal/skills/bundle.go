// Package skills owns Mesh's generic embedded skill and explicit managed installs.
package skills

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed mesh
var embedded embed.FS

type File struct {
	Path string
	Data []byte
}

func Bundle() []File {
	files := []File{}
	_ = fs.WalkDir(embedded, "mesh", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk embedded skill: %w", err)
		}
		if entry.IsDir() {
			return nil
		}
		data, err := embedded.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read embedded skill: %w", err)
		}
		files = append(files, File{Path: strings.TrimPrefix(path, "mesh/"), Data: data})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

func Digest() string { return bundleDigest(Bundle()) }
func bundleDigest(files []File) string {
	hash := sha256.New()
	for _, file := range files {
		_, _ = fmt.Fprintf(hash, "%s\x00%d\x00", file.Path, len(file.Data))
		_, _ = hash.Write(file.Data)
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}
func Show(path string) ([]byte, error) {
	if path == "" {
		path = "SKILL.md"
	}
	for _, file := range Bundle() {
		if file.Path == path {
			return file.Data, nil
		}
	}
	return nil, fmt.Errorf("no embedded skill file %q", path)
}
