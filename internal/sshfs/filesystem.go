// Package sshfs exposes the serving registry as a read-only SSH filesystem.
package sshfs

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/serve"
)

// pkg/sftp maps errno values to permission status codes, unlike fs.ErrPermission.
const permissionDenied = syscall.EACCES

type Filesystem struct {
	registry *serve.Registry
}

// New uses the live HTTP registry, including subsequent service replacements.
func New(registry *serve.Registry) *Filesystem { return &Filesystem{registry: registry} }

type location struct {
	name     string
	service  serve.Service
	relative string
	children []string
}

func (f *Filesystem) locate(name string) (location, error) {
	if strings.ContainsAny(name, "\\\x00") {
		return location{}, permissionDenied
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == ".." {
			return location{}, permissionDenied
		}
	}
	name = strings.TrimPrefix(path.Clean("/"+name), "/")
	loc := location{name: name}
	children := make(map[string]bool)
	for _, service := range f.registry.Services() {
		if service.Kind != serve.Static && service.Kind != serve.Files {
			continue
		}
		loc.include(service, children)
	}
	for child := range children {
		loc.children = append(loc.children, child)
	}
	sort.Strings(loc.children)
	if name != "" && loc.service.Name == "" && len(loc.children) == 0 {
		return location{}, permissionDenied
	}
	return loc, nil
}

func (loc *location) include(service serve.Service, children map[string]bool) {
	prefix := loc.name + "/"
	if loc.name == "" {
		prefix = ""
	}
	if strings.HasPrefix(service.Name, prefix) {
		child, _, _ := strings.Cut(strings.TrimPrefix(service.Name, prefix), "/")
		children[child] = true
	}
	if loc.name != service.Name && !strings.HasPrefix(loc.name, service.Name+"/") {
		return
	}
	if len(service.Name) > len(loc.service.Name) {
		loc.service = service
		loc.relative = strings.TrimPrefix(strings.TrimPrefix(loc.name, service.Name), "/")
	}
}

func (loc location) synthetic() bool { return loc.service.Name == "" }

func (loc location) intermediate() bool {
	return loc.name != loc.service.Name && len(loc.children) > 0
}

func (f *Filesystem) open(name string) (*os.File, fs.FileInfo, error) {
	loc, err := f.locate(name)
	if err != nil {
		return nil, nil, err
	}
	return loc.open()
}

func (loc location) open() (*os.File, fs.FileInfo, error) {
	if loc.synthetic() {
		return nil, nil, permissionDenied
	}
	file, info, err := serve.LiteralRootPath(loc.relative).Open(loc.service.Target)
	if err != nil {
		return nil, nil, clientError(err)
	}
	if loc.intermediate() && !info.IsDir() {
		_ = file.Close()
		return nil, nil, permissionDenied
	}
	return file, namedInfo{FileInfo: info, name: path.Base(loc.name)}, nil
}

func (f *Filesystem) stat(name string, follow bool) (fs.FileInfo, error) {
	loc, err := f.locate(name)
	if err != nil {
		return nil, err
	}
	if loc.synthetic() || loc.intermediate() {
		return directoryInfo{name: path.Base("/" + loc.name)}, nil
	}
	if !follow {
		info, err := serve.LiteralRootPath(loc.relative).Lstat(loc.service.Target)
		if err != nil {
			return nil, clientError(err)
		}
		return namedInfo{FileInfo: info, name: path.Base(loc.name)}, nil
	}
	file, info, err := loc.open()
	if err != nil {
		return nil, err
	}
	return info, clientError(file.Close())
}

func (f *Filesystem) readDir(name string) ([]fs.FileInfo, error) {
	reader, err := f.openDir(name)
	if err != nil {
		return nil, err
	}
	return reader.fileList, reader.Close()
}

type directoryReader struct {
	fileList
	file      *os.File
	info      fs.FileInfo
	synthetic bool
}

func (reader *directoryReader) Close() error {
	if reader.file == nil {
		return nil
	}
	return clientError(reader.file.Close())
}

func (reader *directoryReader) Stat() (fs.FileInfo, error) {
	if reader.synthetic {
		return reader.info, nil
	}
	info, err := reader.file.Stat()
	if err != nil {
		return nil, clientError(err)
	}
	return namedInfo{FileInfo: info, name: reader.info.Name()}, nil
}

func (f *Filesystem) openDir(name string) (*directoryReader, error) {
	loc, err := f.locate(name)
	if err != nil {
		return nil, err
	}
	reader := &directoryReader{
		info: directoryInfo{name: path.Base("/" + loc.name)}, synthetic: loc.synthetic() || loc.intermediate(),
	}
	entries := make(map[string]fs.FileInfo)
	if !loc.synthetic() {
		reader.file, err = f.readRootDir(loc, entries)
		if err != nil && !loc.intermediate() {
			return nil, err
		}
	}
	for _, child := range loc.children {
		entries[child] = directoryInfo{name: child}
	}
	reader.fileList = make(fileList, 0, len(entries))
	for _, info := range entries {
		reader.fileList = append(reader.fileList, info)
	}
	sort.Slice(reader.fileList, func(i, j int) bool { return reader.fileList[i].Name() < reader.fileList[j].Name() })
	return reader, nil
}

func (f *Filesystem) readRootDir(loc location, entries map[string]fs.FileInfo) (*os.File, error) {
	file, _, err := loc.open()
	if err != nil {
		return nil, err
	}
	children, err := file.ReadDir(-1)
	if err != nil {
		_ = file.Close()
		return nil, clientError(err)
	}
	for _, child := range children {
		info, err := serve.LiteralRootPath(path.Join(loc.relative, child.Name())).Lstat(loc.service.Target)
		if err != nil || !regularEntry(info) {
			continue
		}
		entries[child.Name()] = namedInfo{FileInfo: info, name: child.Name()}
	}
	return file, nil
}

func regularEntry(info fs.FileInfo) bool {
	return info.IsDir() || info.Mode().IsRegular() || info.Mode()&fs.ModeSymlink != 0
}

// Resolver errors can contain host paths. Only protocol-safe errors leave SSH.
func clientError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, serve.ErrOutsideRoot) || errors.Is(err, serve.ErrInvalidPath) || errors.Is(err, fs.ErrPermission) {
		return permissionDenied
	}
	if errors.Is(err, fs.ErrNotExist) {
		return syscall.ENOENT
	}
	return permissionDenied
}

type namedInfo struct {
	fs.FileInfo
	name string
}

func (info namedInfo) Name() string      { return info.name }
func (info namedInfo) Mode() fs.FileMode { return info.FileInfo.Mode() &^ 0o222 }
func (info namedInfo) Sys() any          { return nil }

type directoryInfo struct{ name string }

func (info directoryInfo) Name() string  { return info.name }
func (directoryInfo) Size() int64        { return 0 }
func (directoryInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o555 }
func (directoryInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (directoryInfo) IsDir() bool        { return true }
func (directoryInfo) Sys() any           { return nil }
