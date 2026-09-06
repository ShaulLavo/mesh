package sshfs

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	charmssh "charm.land/ssh"
	"charm.land/wish/v2/scp"
)

// Middleware dispatches SCP before the SSH terminal command parser.
func (f *Filesystem) Middleware(next charmssh.Handler) charmssh.Handler {
	copyFiles := scp.Middleware(f, nil)(next)
	return func(session charmssh.Session) {
		arguments := session.Command()
		if len(arguments) == 0 || arguments[0] != "scp" {
			next(session)
			return
		}
		defer func() { _ = session.Close() }()
		command, err := scpCommand(arguments)
		if err != nil {
			_, _ = fmt.Fprintln(session.Stderr(), "scp: read-only served roots: permission denied")
			_ = session.Exit(1)
			return
		}
		copyFiles(scpSession{Session: session, command: command})
		_ = session.Exit(0)
	}
}

type scpSession struct {
	charmssh.Session
	command []string
}

func (s scpSession) Command() []string { return s.command }

func scpCommand(arguments []string) ([]string, error) {
	flags := ""
	index := 1
	for index < len(arguments) && strings.HasPrefix(arguments[index], "-") {
		argument := arguments[index]
		index++
		if argument == "--" {
			break
		}
		flags += strings.TrimPrefix(argument, "-")
	}
	if index != len(arguments)-1 || arguments[index] == "" || strings.ContainsAny(arguments[index], "\r\n\x00") {
		return nil, permissionDenied
	}
	if strings.Count(flags, "f") != 1 || strings.Trim(flags, "frpqvd") != "" {
		return nil, permissionDenied
	}
	command := []string{"scp"}
	if strings.Contains(flags, "r") {
		command = append(command, "-r")
	}
	return append(command, "-f", "/"+strings.TrimLeft(arguments[index], "/")), nil
}

func (f *Filesystem) Glob(_ charmssh.Session, name string) ([]string, error) {
	loc, err := f.locate(name)
	if err != nil {
		return nil, err
	}
	return []string{"/" + loc.name}, nil
}

func (f *Filesystem) WalkDir(session charmssh.Session, name string, visit fs.WalkDirFunc) error {
	return f.walkDir(session, name, visit, make(map[string]bool))
}

func (f *Filesystem) walkDir(session charmssh.Session, name string, visit fs.WalkDirFunc, ancestors map[string]bool) error {
	if len(ancestors) > 128 || session.Context().Err() != nil {
		return permissionDenied
	}
	info, err := f.stat(name, true)
	if err != nil {
		return err
	}
	err = visit(name, fs.FileInfoToDirEntry(info), nil)
	if err == fs.SkipDir && info.IsDir() {
		return nil
	}
	if err != nil || !info.IsDir() {
		return err
	}
	canonical, err := f.RealPath(name)
	if err != nil {
		return err
	}
	if ancestors[canonical] {
		return permissionDenied
	}
	ancestors[canonical] = true
	defer delete(ancestors, canonical)
	entries, err := f.readDir(name)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := f.walkDir(session, path.Join(name, entry.Name()), visit, ancestors); err != nil {
			return err
		}
	}
	return nil
}

func (f *Filesystem) NewDirEntry(_ charmssh.Session, name string) (*scp.DirEntry, error) {
	info, err := f.stat(name, true)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || !scpName(info.Name()) {
		return nil, permissionDenied
	}
	return &scp.DirEntry{Name: info.Name(), Filepath: name, Mode: info.Mode()}, nil
}

func (f *Filesystem) NewFileEntry(_ charmssh.Session, name string) (*scp.FileEntry, func() error, error) {
	file, info, err := f.open(name)
	if err != nil {
		return nil, nil, err
	}
	if err := file.Close(); err != nil {
		return nil, nil, clientError(err)
	}
	if !info.Mode().IsRegular() || !scpName(info.Name()) {
		return nil, nil, permissionDenied
	}
	reader := &scpReader{filesystem: f, name: name, remaining: info.Size()}
	return &scp.FileEntry{
		Name: info.Name(), Filepath: name, Mode: info.Mode(), Size: info.Size(), Reader: reader,
		Mtime: info.ModTime().Unix(), Atime: info.ModTime().Unix(),
	}, reader.Close, nil
}

func scpName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\r\n\x00")
}

// Wish builds the transfer tree before sending. Open one file at a time and
// send exactly its advertised size, even if another process appends to it.
type scpReader struct {
	filesystem *Filesystem
	name       string
	remaining  int64
	file       *os.File
}

func (reader *scpReader) Read(buffer []byte) (int, error) {
	if reader.remaining == 0 {
		return 0, io.EOF
	}
	if reader.file == nil {
		file, info, err := reader.filesystem.open(reader.name)
		if err != nil {
			return 0, err
		}
		if !info.Mode().IsRegular() || info.Size() < reader.remaining {
			_ = file.Close()
			return 0, io.ErrUnexpectedEOF
		}
		reader.file = file
	}
	if int64(len(buffer)) > reader.remaining {
		buffer = buffer[:reader.remaining]
	}
	n, err := reader.file.Read(buffer)
	reader.remaining -= int64(n)
	if reader.remaining == 0 {
		return n, reader.Close()
	}
	if err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, clientError(err)
}

func (reader *scpReader) Close() error {
	if reader.file == nil {
		return nil
	}
	err := reader.file.Close()
	reader.file = nil
	return clientError(err)
}
