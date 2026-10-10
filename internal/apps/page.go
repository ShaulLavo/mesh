package apps

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type showSource struct {
	path string
	name string
	dir  bool
}

type showCopy struct {
	limit SizeLimit
	bytes int64
	files int
}

// PreparePage stages selected files as a gallery or an HTML site for the normal app upload.
func PreparePage(ctx context.Context, files []string, limit SizeLimit) (directory string, err error) {
	sources, err := showSources(files)
	if err != nil {
		return "", err
	}
	directory, err = os.MkdirTemp("", "mesh-show-*")
	if err != nil {
		return "", fmt.Errorf("app: create staging directory: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(directory)
		}
	}()
	err = writeShow(ctx, directory, sources, limit)
	return directory, err
}

func showSources(files []string) ([]showSource, error) {
	if len(files) == 0 {
		return nil, errors.New("app: choose at least one file or an HTML directory")
	}
	sources := make([]showSource, 0, len(files))
	for _, file := range files {
		source, err := inspectShowSource(file)
		if err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	return sources, nil
}

func inspectShowSource(file string) (showSource, error) {
	source := showSource{path: filepath.Clean(file), name: filepath.Base(filepath.Clean(file))}
	info, err := os.Lstat(source.path)
	if err != nil {
		return source, fmt.Errorf("app: read selected file: %w", err)
	}
	source.dir = info.IsDir()
	if !source.dir && !info.Mode().IsRegular() {
		return source, fmt.Errorf("app: select a regular file or HTML directory: %s", file)
	}
	if excluded(source.name) || secretName(source.name) {
		return source, fmt.Errorf("app: environment, credential or generated source is excluded: %s", file)
	}
	if !source.dir {
		return source, nil
	}
	index, err := os.Lstat(filepath.Join(source.path, "index.html"))
	if err != nil || !index.Mode().IsRegular() {
		return source, fmt.Errorf("app: directory must contain a regular index.html: %s", file)
	}
	return source, nil
}

func writeShow(ctx context.Context, directory string, sources []showSource, limit SizeLimit) error {
	copier := &showCopy{limit: limit}
	if len(sources) == 1 && (sources[0].dir || showHTML(sources[0].name)) {
		destination := directory
		if !sources[0].dir {
			destination = filepath.Join(directory, "index.html")
		}
		return copier.asset(ctx, sources[0], destination)
	}
	page, err := showPageWithAssets(sources, limit)
	if err != nil {
		return err
	}
	if page >= 0 {
		return copier.site(ctx, directory, sources, page)
	}
	var sections strings.Builder
	for i, source := range sources {
		asset := fmt.Sprintf("%d-%s", i, source.name)
		if err := copier.asset(ctx, source, filepath.Join(directory, asset)); err != nil {
			return err
		}
		sections.WriteString(showSection(source, asset))
	}
	pageHTML := `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>` + html.EscapeString(sources[0].name) + `</title>
<style>:root{color-scheme:light dark}body{font:16px/1.55 system-ui,sans-serif;max-width:860px;margin:2rem auto;padding:0 1rem}img,video{max-width:100%}figure{margin:1.5rem 0}figcaption{font-size:.85rem}</style></head><body><main>` + sections.String() + `</main></body></html>`
	if err := os.WriteFile(filepath.Join(directory, "index.html"), []byte(pageHTML), 0600); err != nil {
		return fmt.Errorf("app: write gallery: %w", err)
	}
	return nil
}

func showHTML(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".html" || ext == ".htm"
}

func showPageWithAssets(sources []showSource, limit SizeLimit) (int, error) {
	page := -1
	names := make(map[string]bool, len(sources))
	for i, source := range sources {
		if names[source.name] {
			return -1, nil
		}
		names[source.name] = true
		if source.dir || !showHTML(source.name) {
			continue
		}
		if page >= 0 {
			return -1, nil
		}
		page = i
	}
	if page < 0 {
		return -1, nil
	}
	content, err := readShowPage(sources[page].path, limit)
	if err != nil {
		return -1, err
	}
	for i, source := range sources {
		if i != page && !strings.Contains(string(content), source.name) {
			return -1, nil
		}
	}
	return page, nil
}

func readShowPage(file string, limit SizeLimit) ([]byte, error) {
	f, err := os.Open(file) //nolint:gosec // Read the explicitly selected HTML file; size is bounded before staging.
	if err != nil {
		return nil, fmt.Errorf("app: open HTML page: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(limit.Reader(f))
	if err != nil {
		return nil, fmt.Errorf("app: read HTML page: %w", err)
	}
	if limit.Exceeded(int64(len(data))) {
		return nil, errSourceTooLarge
	}
	return data, nil
}

func (c *showCopy) site(ctx context.Context, directory string, sources []showSource, page int) error {
	for i, source := range sources {
		name := source.name
		if i == page {
			name = "index.html"
		}
		if i != page && name == "index.html" {
			return errors.New("app: an asset named index.html conflicts with the selected page")
		}
		if err := c.asset(ctx, source, filepath.Join(directory, name)); err != nil {
			return err
		}
	}
	return nil
}

func showSection(source showSource, asset string) string {
	href := url.PathEscape(asset)
	if source.dir {
		href += "/index.html"
	}
	href, label := html.EscapeString(href), html.EscapeString(source.name)
	if source.dir || showHTML(source.name) {
		return `<p><a href="` + href + `">` + label + `</a></p>` + "\n"
	}
	switch strings.ToLower(filepath.Ext(source.name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".avif":
		return `<figure><img src="` + href + `" alt="` + label + `"><figcaption>` + label + `</figcaption></figure>` + "\n"
	case ".mp4", ".webm", ".mov":
		return `<figure><video src="` + href + `" controls></video><figcaption>` + label + `</figcaption></figure>` + "\n"
	default:
		return `<p><a href="` + href + `">` + label + `</a></p>` + "\n"
	}
}

func (c *showCopy) asset(ctx context.Context, source showSource, destination string) error {
	rootPath, name := filepath.Dir(source.path), source.name
	if source.dir {
		rootPath, name = source.path, "."
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return fmt.Errorf("app: open source directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	if !source.dir {
		return c.file(ctx, root, name, destination)
	}
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		return c.entry(ctx, root, destination, name, entry, walkErr)
	})
	if err != nil {
		return fmt.Errorf("app: copy selected directory: %w", err)
	}
	return nil
}

func (c *showCopy) entry(ctx context.Context, root *os.Root, destination, name string, entry fs.DirEntry, walkErr error) error {
	if walkErr != nil {
		return walkErr
	}
	if excluded(name) && entry.IsDir() {
		return fs.SkipDir
	}
	if excluded(name) || entry.IsDir() {
		return nil
	}
	if secretName(name) || !entry.Type().IsRegular() {
		return fmt.Errorf("app: unsupported source file: %s", name)
	}
	return c.file(ctx, root, name, filepath.Join(destination, name))
}

func (c *showCopy) file(ctx context.Context, root *os.Root, name, destination string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("app: prepare selected file: %w", err)
	}
	f, err := root.Open(name)
	if err != nil {
		return fmt.Errorf("app: open source: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("app: inspect source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("app: source must be a regular file")
	}
	c.bytes += info.Size()
	c.files++
	if c.limit.Exceeded(c.bytes) || c.files > maxSourceFiles {
		return errSourceTooLarge
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return fmt.Errorf("app: create asset directory: %w", err)
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600) //nolint:gosec // Destination is inside a fresh staging directory with generated asset names.
	if err != nil {
		return fmt.Errorf("app: create asset: %w", err)
	}
	_, copyErr := io.CopyN(out, f, info.Size())
	return errors.Join(copyErr, out.Close())
}
