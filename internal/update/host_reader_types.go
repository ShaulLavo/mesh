package update

import "github.com/shaul/mesh/internal/release"

const (
	HostReaderSchema               = 1
	HostReaderEpoch         uint32 = 1
	MaxHostReaderInputBytes        = 8 << 20
	MaxHostReaderWorkers           = 4096
)

// HostReaderDocuments retains original bytes as base64 JSON byte strings. Page
// is the exact anchor page referenced by the signed metadata, not a route walk.
type HostReaderDocuments struct {
	Metadata []byte `json:"metadata"`
	Envelope []byte `json:"envelope"`
	Manifest []byte `json:"manifest"`
	Page     []byte `json:"page"`
}

type HostReaderWorker struct {
	Digest   string `json:"digest"`
	Protocol int    `json:"protocol"`
}

type HostReaderInput struct {
	Schema   int                 `json:"schema"`
	Source   HostReaderDocuments `json:"source"`
	Target   HostReaderDocuments `json:"target"`
	Forward  []byte              `json:"forward"`
	Rollback []byte              `json:"rollback"`
	Workers  []HostReaderWorker  `json:"workers"`
}

type HostReaderCapabilities struct {
	Schema int           `json:"schema"`
	Epoch  uint32        `json:"epoch"`
	Build  release.Build `json:"build"`
}

type HostReaderResult struct {
	Schema   int              `json:"schema"`
	Source   release.Build    `json:"source"`
	Epoch    uint32           `json:"epoch"`
	Position release.Position `json:"position"`
	Target   string           `json:"target"`
	Metadata string           `json:"metadata"`
	Binary   string           `json:"binary"`
}
