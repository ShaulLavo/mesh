package release

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	prototrust "github.com/sigstore/protobuf-specs/gen/pb-go/trustroot/v1"
	"github.com/sigstore/sigstore-go/pkg/root"
	"google.golang.org/protobuf/proto"
)

type effectiveMaterial struct {
	cas    map[string]*prototrust.CertificateAuthority
	tsas   map[string]*prototrust.CertificateAuthority
	ctlogs map[string]*prototrust.TransparencyLogInstance
	tlogs  map[string]*prototrust.TransparencyLogInstance
}

func effectiveTrust(materials []authenticatedMaterial) (root.TrustedMaterial, error) {
	sort.Slice(materials, func(i, j int) bool { return compareMaterialOrder(materials[i].order, materials[j].order) < 0 })
	effective := effectiveMaterial{cas: make(map[string]*prototrust.CertificateAuthority), tsas: make(map[string]*prototrust.CertificateAuthority), ctlogs: make(map[string]*prototrust.TransparencyLogInstance), tlogs: make(map[string]*prototrust.TransparencyLogInstance)}
	for index, material := range materials {
		if index > 0 && material.order == materials[index-1].order && !bytes.Equal(material.contents, materials[index-1].contents) {
			return nil, errors.New("release: conflicting material at the same authenticated revision")
		}
		if err := effective.add(material.contents); err != nil {
			return nil, err
		}
	}
	combined := &prototrust.TrustedRoot{MediaType: root.TrustedRootMediaType01, CertificateAuthorities: orderedTrustValues(effective.cas), TimestampAuthorities: orderedTrustValues(effective.tsas), Ctlogs: orderedTrustValues(effective.ctlogs), Tlogs: orderedTrustValues(effective.tlogs)}
	result, err := root.NewTrustedRootFromProtobuf(combined)
	if err != nil {
		return nil, fmt.Errorf("release: construct effective trust material: %w", err)
	}
	return result, nil
}

func compareMaterialOrder(a, b materialOrder) int {
	if a.Root < b.Root {
		return -1
	}
	if a.Root > b.Root {
		return 1
	}
	if a.Snapshot < b.Snapshot {
		return -1
	}
	if a.Snapshot > b.Snapshot {
		return 1
	}
	return 0
}

func (effective effectiveMaterial) add(contents []byte) error {
	material, err := root.NewTrustedRootProtobuf(contents)
	if err != nil {
		return fmt.Errorf("release: parse authenticated trust protobuf: %w", err)
	}
	if _, err := root.NewTrustedRootFromProtobuf(material); err != nil {
		return fmt.Errorf("release: validate authenticated trust material: %w", err)
	}
	if err := replaceAuthorities(effective.cas, material.CertificateAuthorities); err != nil {
		return err
	}
	if err := replaceAuthorities(effective.tsas, material.TimestampAuthorities); err != nil {
		return err
	}
	if err := replaceLogs(effective.ctlogs, material.Ctlogs); err != nil {
		return err
	}
	return replaceLogs(effective.tlogs, material.Tlogs)
}

// Identity includes the exact certificate chain and URI, but excludes mutable validity windows.
func replaceAuthorities(destination map[string]*prototrust.CertificateAuthority, authorities []*prototrust.CertificateAuthority) error {
	seen := make(map[string]bool, len(authorities))
	for _, authority := range authorities {
		identity, err := proto.MarshalOptions{Deterministic: true}.Marshal(&prototrust.CertificateAuthority{Uri: authority.GetUri(), CertChain: authority.GetCertChain()})
		if err != nil {
			return fmt.Errorf("release: identify trust authority: %w", err)
		}
		key := string(identity)
		if seen[key] {
			return errors.New("release: duplicate authority definition in one material revision")
		}
		seen[key] = true
		destination[key] = authority
	}
	return nil
}

func replaceLogs(destination map[string]*prototrust.TransparencyLogInstance, logs []*prototrust.TransparencyLogInstance) error {
	seen := make(map[string]bool, len(logs))
	for _, log := range logs {
		key := hex.EncodeToString(log.GetLogId().GetKeyId())
		if key == "" || seen[key] {
			return errors.New("release: missing or duplicate log identity in one material revision")
		}
		seen[key] = true
		destination[key] = log
	}
	return nil
}

func orderedTrustValues[T any](entries map[string]T) []T {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]T, 0, len(keys))
	for _, key := range keys {
		values = append(values, entries[key])
	}
	return values
}
