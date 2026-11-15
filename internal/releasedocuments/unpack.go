package releasedocuments

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"

	"github.com/shaul/mesh/internal/release"
)

func unpack(ctx context.Context, archivePath string, envelope []byte, expected *Binding, trust release.TrustRoot, destination string) (Inventory, error) {
	var result Inventory
	compressed, err := os.Open(archivePath) //nolint:gosec // Operation-owned fixed input.
	if err != nil {
		return result, documentError(err)
	}
	defer func() { _ = compressed.Close() }()
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(contextReader{ctx, compressed}, MaxCompressed+1))
	if err != nil {
		return result, documentError(err)
	}
	if size > MaxCompressed {
		return result, errors.New("documents: compressed archive exceeds bound")
	}
	if err := release.VerifyArtifactSignature(trust, hex.EncodeToString(hash.Sum(nil)), envelope); err != nil {
		return result, documentError(err)
	}
	if _, err := compressed.Seek(0, io.SeekStart); err != nil {
		return result, documentError(err)
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return result, documentError(err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.OpenFile(".archive.tar", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return result, documentError(err)
	}
	defer func() { _ = file.Close(); _ = root.Remove(".archive.tar") }()
	if err := expand(ctx, compressed, file); err != nil {
		return result, documentError(err)
	}
	result, err = extract(ctx, file, root, expected)
	if err != nil {
		return result, documentError(err)
	}
	return result, validateIdentity(ctx, root, result.Binding)
}

func expand(ctx context.Context, compressed io.Reader, output *os.File) error {
	buffered := bufio.NewReader(contextReader{ctx, compressed})
	reader, err := gzip.NewReader(buffered)
	if err != nil {
		return documentError(err)
	}
	defer func() { _ = reader.Close() }()
	reader.Multistream(false)
	size, err := io.Copy(output, io.LimitReader(contextReader{ctx, reader}, MaxExpanded+1))
	if err != nil {
		return documentError(err)
	}
	if size > MaxExpanded {
		return errors.New("documents: expanded archive exceeds bound")
	}
	if _, err := buffered.ReadByte(); !errors.Is(err, io.EOF) {
		return errors.New("documents: trailing gzip data")
	}
	return nil
}

func extract(ctx context.Context, file *os.File, root *os.Root, expected *Binding) (Inventory, error) {
	var result Inventory
	header, err := readHeader(file, 0)
	if err != nil {
		return result, documentError(err)
	}
	if header.Name != catalogName || header.Size <= 0 || header.Size > maxCatalog {
		return result, errors.New("documents: first member must be bounded catalog")
	}
	data, err := readSection(ctx, file, 512, header.Size)
	if err != nil {
		return result, documentError(err)
	}
	var value catalog
	if err := decodeJSON(ctx, data, &value); err != nil {
		return result, documentError(err)
	}
	if err := validateCatalog(value, expected); err != nil {
		return result, documentError(err)
	}
	if err := padding(file, 512+header.Size, aligned(header.Size)-header.Size); err != nil {
		return result, documentError(err)
	}
	offset := 512 + aligned(header.Size)
	for _, document := range value.Documents {
		if err := extractMember(ctx, file, root, offset, document); err != nil {
			return result, documentError(err)
		}
		offset += 512 + aligned(document.Size)
	}
	info, err := file.Stat()
	if err != nil {
		return result, documentError(err)
	}
	if info.Size() != offset+1024 {
		return result, errors.New("documents: missing end blocks or trailing tar data")
	}
	if err := padding(file, offset, 1024); err != nil {
		return result, documentError(err)
	}
	return Inventory{Schema: 1, Binding: value.Binding, Documents: value.Documents}, nil
}

func validateCatalog(value catalog, expected *Binding) error {
	if value.Schema != 1 || len(value.Documents) < 3 || len(value.Documents) >= MaxMembers {
		return errors.New("documents: invalid catalog format or count")
	}
	if err := validateBinding(value.Binding); err != nil {
		return documentError(err)
	}
	if expected != nil && !sameBinding(value.Binding, *expected) {
		return errors.New("documents: catalog differs from expected release binding")
	}
	for index, document := range value.Documents {
		if err := checkDocument(document); err != nil {
			return documentError(err)
		}
		if index > 0 && value.Documents[index-1].Name >= document.Name {
			return errors.New("documents: catalog names must be unique and sorted")
		}
	}
	return requiredMembers(value.Documents)
}

func readHeader(file *os.File, offset int64) (*tar.Header, error) {
	block := make([]byte, 512)
	if _, err := file.ReadAt(block, offset); err != nil {
		return nil, documentError(err)
	}
	if block[156] != tar.TypeReg || !bytes.Equal(block[257:265], []byte{'u', 's', 't', 'a', 'r', 0, '0', '0'}) {
		return nil, errors.New("documents: only regular USTAR entries allowed")
	}
	header, err := tar.NewReader(bytes.NewReader(block)).Next()
	if err != nil {
		return nil, documentError(err)
	}
	if header.Format != tar.FormatUSTAR || header.Linkname != "" || header.Name != catalogName && !validName(header.Name) {
		return nil, errors.New("documents: invalid member header")
	}
	return header, nil
}

func extractMember(ctx context.Context, file *os.File, root *os.Root, offset int64, document Document) error {
	header, err := readHeader(file, offset)
	if err != nil {
		return documentError(err)
	}
	if header.Name != document.Name || header.Size != document.Size {
		return errors.New("documents: member differs from catalog")
	}
	data, err := readSection(ctx, file, offset+512, document.Size)
	if err != nil {
		return documentError(err)
	}
	if digest(data) != document.SHA256 {
		return errors.New("documents: member hash differs from catalog")
	}
	if err := padding(file, offset+512+document.Size, aligned(document.Size)-document.Size); err != nil {
		return documentError(err)
	}
	return writeFile(ctx, root, document.Name, data)
}

func readSection(ctx context.Context, file *os.File, offset, size int64) ([]byte, error) {
	data, err := io.ReadAll(contextReader{ctx, io.NewSectionReader(file, offset, size)})
	if err == nil && int64(len(data)) != size {
		err = io.ErrUnexpectedEOF
	}
	return data, documentError(err)
}

func aligned(size int64) int64 { return (size + 511) / 512 * 512 }
func padding(file *os.File, offset, size int64) error {
	data, err := io.ReadAll(io.NewSectionReader(file, offset, size))
	if err != nil {
		return documentError(err)
	}
	if int64(len(data)) != size || !bytes.Equal(data, make([]byte, size)) {
		return errors.New("documents: invalid zero padding")
	}
	return nil
}
