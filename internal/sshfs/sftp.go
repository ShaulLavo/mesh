package sshfs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"

	charmssh "charm.land/ssh"
	"github.com/pkg/sftp"
	"github.com/shaul/mesh/internal/serve"
)

func (f *Filesystem) Subsystem(session charmssh.Session) {
	server := sftp.NewRequestServer(guardRequests(session), sftp.Handlers{
		FileGet: f, FilePut: f, FileCmd: f, FileList: f,
	})
	err := server.Serve()
	status := 0
	if err != nil && !errors.Is(err, io.EOF) {
		status = 1
	}
	_ = session.Exit(status)
	_ = server.Close()
}

func (f *Filesystem) Fileread(request *sftp.Request) (io.ReaderAt, error) {
	flags := request.Pflags()
	if !flags.Read || flags.Write || flags.Append || flags.Creat || flags.Trunc || flags.Excl {
		return nil, permissionDenied
	}
	file, info, err := f.open(request.Filepath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, permissionDenied
	}
	return &fileReader{file: file, name: info.Name()}, nil
}

// A descriptor can return PathError after opening, including on invalid offsets.
// Keep the host pathname out of status replies throughout the handle's lifetime.
type fileReader struct {
	file *os.File
	name string
}

func (reader *fileReader) ReadAt(buffer []byte, offset int64) (int, error) {
	n, err := reader.file.ReadAt(buffer, offset)
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	return n, clientError(err)
}

func (reader *fileReader) Close() error { return clientError(reader.file.Close()) }

func (reader *fileReader) Stat() (fs.FileInfo, error) {
	info, err := reader.file.Stat()
	if err != nil {
		return nil, clientError(err)
	}
	return namedInfo{FileInfo: info, name: reader.name}, nil
}

func (*Filesystem) Filewrite(*sftp.Request) (io.WriterAt, error) { return nil, permissionDenied }
func (*Filesystem) Filecmd(*sftp.Request) error                  { return permissionDenied }

func (f *Filesystem) Filelist(request *sftp.Request) (sftp.ListerAt, error) {
	if request.Method == "List" {
		return f.openDir(request.Filepath)
	}
	if request.Method != "Stat" {
		return nil, permissionDenied
	}
	info, err := f.stat(request.Filepath, true)
	if err != nil {
		return nil, err
	}
	return fileList{info}, nil
}

func (f *Filesystem) Lstat(request *sftp.Request) (sftp.ListerAt, error) {
	info, err := f.stat(request.Filepath, false)
	if err != nil {
		return nil, err
	}
	return fileList{info}, nil
}

func (f *Filesystem) RealPath(name string) (string, error) {
	loc, err := f.locate(name)
	if err != nil {
		return "", err
	}
	if loc.synthetic() || loc.intermediate() {
		return "/" + loc.name, nil
	}
	relative, err := serve.LiteralRootPath(loc.relative).ResolveRelative(loc.service.Target)
	if err != nil {
		return "", clientError(err)
	}
	return f.canonicalPath(loc, relative)
}

func (f *Filesystem) Readlink(name string) (string, error) {
	loc, err := f.locate(name)
	if err != nil {
		return "", err
	}
	if loc.synthetic() || loc.intermediate() {
		return "", permissionDenied
	}
	relative, err := serve.LiteralRootPath(loc.relative).Readlink(loc.service.Target)
	if err != nil {
		return "", clientError(err)
	}
	return f.canonicalPath(loc, relative)
}

func (f *Filesystem) canonicalPath(source location, relative string) (string, error) {
	name := path.Join("/", source.service.Name, relative)
	target, err := f.locate(name)
	if err != nil {
		return "", err
	}
	if target.service.Name != source.service.Name || target.service.Target != source.service.Target || target.intermediate() {
		return "", permissionDenied
	}
	return name, nil
}

type fileList []fs.FileInfo

func (list fileList) ListAt(destination []fs.FileInfo, offset int64) (int, error) {
	if offset < 0 {
		return 0, fs.ErrInvalid
	}
	if offset >= int64(len(list)) {
		return 0, io.EOF
	}
	n := copy(destination, list[offset:])
	if n < len(destination) {
		return n, io.EOF
	}
	return n, nil
}
