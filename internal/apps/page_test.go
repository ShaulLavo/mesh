package apps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pageFile(t *testing.T, root, name, content string) string {
	t.Helper()
	file := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

func preparedPage(t *testing.T, files ...string) string {
	t.Helper()
	directory, err := PreparePage(context.Background(), files, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

func pageContent(t *testing.T, directory, name string) string {
	t.Helper()
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestPreparePageGalleryPreservesAssetsAndEscapesNames(t *testing.T) {
	root := t.TempDir()
	image := pageFile(t, root, "shot & photo.png", "image")
	video := pageFile(t, root, "clip.webm", "video")
	page := pageFile(t, root, "mock.html", "<p>mock</p>")
	report := pageFile(t, root, "report.md", "report")
	site := filepath.Join(root, "interactive")
	pageFile(t, site, "index.html", `<img src="image.png">`)
	pageFile(t, site, "image.png", "nested image")
	directory := preparedPage(t, image, video, page, report, site)
	content := pageContent(t, directory, "index.html")
	for _, fragment := range []string{"shot &amp; photo.png", `src="0-shot%20&amp;%20photo.png"`, `<video src="1-clip.webm" controls>`, `href="2-mock.html"`, `href="3-report.md"`, `href="4-interactive/index.html"`} {
		if !strings.Contains(content, fragment) {
			t.Fatalf("gallery missing %q: %s", fragment, content)
		}
	}
	if pageContent(t, directory, "4-interactive/image.png") != "nested image" || pageContent(t, directory, "0-shot & photo.png") != "image" {
		t.Fatal("asset contents changed")
	}
}

func TestPreparePageHTMLAndRelativeAssets(t *testing.T) {
	root := t.TempDir()
	html := `<link rel="stylesheet" href="style.css"><img src="image.png">`
	page := pageFile(t, root, "mock.html", html)
	style := pageFile(t, root, "style.css", "body{color:red}")
	image := pageFile(t, root, "image.png", "image")
	directory := preparedPage(t, page, style, image)
	if pageContent(t, directory, "index.html") != html || pageContent(t, directory, "style.css") != "body{color:red}" || pageContent(t, directory, "image.png") != "image" {
		t.Fatal("HTML site lost its page or relative assets")
	}
	single := preparedPage(t, page)
	if pageContent(t, single, "index.html") != html {
		t.Fatal("single HTML page changed")
	}
}

func TestPreparePageDuplicateNamesRemainSeparate(t *testing.T) {
	root := t.TempDir()
	first := pageFile(t, root, "first/image.png", "first")
	second := pageFile(t, root, "second/image.png", "second")
	directory := preparedPage(t, first, second)
	if pageContent(t, directory, "0-image.png") != "first" || pageContent(t, directory, "1-image.png") != "second" {
		t.Fatal("duplicate names overwrote each other")
	}
}

func TestPreparePageRefusesUnsafeFilesAndCleansFailedStaging(t *testing.T) {
	root := t.TempDir()
	staging := t.TempDir()
	t.Setenv("TMPDIR", staging)
	t.Setenv("TMP", staging)
	t.Setenv("TEMP", staging)
	image := pageFile(t, root, "image.png", "image")
	link := filepath.Join(root, "link.png")
	if err := os.Symlink(image, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	site := filepath.Join(root, "site")
	pageFile(t, site, "index.html", "page")
	if err := os.Symlink(image, filepath.Join(site, "nested.png")); err != nil {
		t.Fatal(err)
	}
	for _, files := range [][]string{nil, {link}, {site}, {pageFile(t, root, ".env", "secret")}, {pageFile(t, root, "private.key", "secret")}, {filepath.Join(root, "missing")}} {
		if _, err := PreparePage(context.Background(), files, 0); err == nil {
			t.Fatalf("accepted unsafe selection: %v", files)
		}
	}
	entries, err := os.ReadDir(staging)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed staging left files: %v %v", entries, err)
	}
}

func TestPreparePageUsesAppSourceExclusionsAndLimits(t *testing.T) {
	root := t.TempDir()
	pageFile(t, root, "index.html", "page")
	pageFile(t, root, ".env", "secret")
	pageFile(t, root, "node_modules/dependency.js", "generated")
	directory := preparedPage(t, root)
	if _, err := os.Stat(filepath.Join(directory, ".env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("environment file copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "node_modules")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("generated dependencies copied: %v", err)
	}
	large := pageFile(t, t.TempDir(), "large.png", "")
	if err := os.Truncate(large, MaxArchive+1); err != nil {
		t.Fatal(err)
	}
	if _, err := PreparePage(context.Background(), []string{large}, 0); !errors.Is(err, errSourceTooLarge) {
		t.Fatalf("oversized source accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PreparePage(ctx, []string{filepath.Join(root, "index.html")}, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled preparation accepted: %v", err)
	}
}
