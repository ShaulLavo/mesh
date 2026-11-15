package releasedocuments

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
)

func Pack(ctx context.Context, directory string, members []string, binding Binding, output io.Writer) (Packed, error) {
	var result Packed
	if err := validateBinding(binding); err != nil {
		return result, documentError(err)
	}
	if len(members) < 3 || len(members) >= MaxMembers {
		return result, errors.New("documents: member count exceeds bounds")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return result, documentError(err)
	}
	defer func() { _ = root.Close() }()
	if err := validateIdentity(ctx, root, binding); err != nil {
		return result, documentError(err)
	}
	catalog, err := packCatalog(ctx, root, members, binding)
	if err != nil {
		return result, err
	}
	return encodeArchive(ctx, root, catalog, output)
}

func packCatalog(ctx context.Context, root *os.Root, members []string, binding Binding) (catalog, error) {
	names := slices.Clone(members)
	slices.Sort(names)
	catalog := catalog{Schema: 1, Binding: binding}
	for index, name := range names {
		if !validName(name) || index > 0 && name == names[index-1] {
			return catalog, errors.New("documents: invalid or repeated member")
		}
		data, err := readFile(ctx, root, name, documentLimit(name))
		if err != nil {
			return catalog, documentError(err)
		}
		catalog.Documents = append(catalog.Documents, Document{Name: name, SHA256: digest(data), Size: int64(len(data))})
	}
	return catalog, requiredMembers(catalog.Documents)
}

func encodeArchive(ctx context.Context, root *os.Root, catalog catalog, output io.Writer) (Packed, error) {
	var result Packed
	encoded, err := json.Marshal(catalog)
	if err != nil || len(encoded) > maxCatalog {
		return result, errors.New("documents: catalog exceeds bound")
	}
	expanded := int64(512+1024) + aligned(int64(len(encoded)))
	for _, document := range catalog.Documents {
		expanded += 512 + aligned(document.Size)
	}
	if expanded > MaxExpanded {
		return result, errors.New("documents: expanded archive exceeds bound")
	}
	bounded := &boundedWriter{writer: output, remaining: MaxCompressed}
	compressed := gzip.NewWriter(bounded)
	archive := tar.NewWriter(compressed)
	if err := writeMember(archive, catalogName, encoded); err != nil {
		return result, documentError(err)
	}
	for _, document := range catalog.Documents {
		if err := packMember(ctx, root, archive, document); err != nil {
			return result, documentError(err)
		}
	}
	if err := archive.Close(); err != nil {
		return result, documentError(err)
	}
	if err := compressed.Close(); err != nil {
		return result, documentError(err)
	}
	return Packed{Inventory: Inventory{Schema: 1, Binding: catalog.Binding, Documents: catalog.Documents}}, nil
}

type boundedWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *boundedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, errors.New("documents: compressed archive exceeds bound")
	}
	n, err := w.writer.Write(data)
	w.remaining -= int64(n)
	return n, documentError(err)
}

func writeMember(writer *tar.Writer, name string, data []byte) error {
	header := &tar.Header{Name: name, Mode: 0600, Size: int64(len(data)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}
	if err := writer.WriteHeader(header); err != nil {
		return documentError(err)
	}
	_, err := writer.Write(data)
	return documentError(err)
}

func packMember(ctx context.Context, root *os.Root, writer *tar.Writer, document Document) error {
	data, err := readFile(ctx, root, document.Name, documentLimit(document.Name))
	if err != nil {
		return documentError(err)
	}
	if int64(len(data)) != document.Size || digest(data) != document.SHA256 {
		return errors.New("documents: source changed while packing")
	}
	return writeMember(writer, document.Name, data)
}

func requiredMembers(documents []Document) error {
	for _, name := range []string{"mesh-release.json", "mesh-update.json", "mesh-update-root.json"} {
		if !slices.ContainsFunc(documents, func(document Document) bool { return document.Name == name }) {
			return errors.New("documents: required identity document missing")
		}
	}
	return nil
}
