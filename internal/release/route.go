package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

type DocumentKind string

const (
	RootDocument     DocumentKind = "root"
	MetadataDocument DocumentKind = "metadata"
	PageDocument     DocumentKind = "page"
	ManifestDocument DocumentKind = "manifest"
	ReceiptDocument  DocumentKind = "receipt"
)

// DocumentSource separates transport from trust. VerifyMetadata must authenticate
// exact metadata bytes with the host's signature verifier; hashes alone do not.
// The offline maintainer command explicitly supplies local evidence instead.
type DocumentSource struct {
	Read           func(context.Context, AssetRef, DocumentKind) ([]byte, error)
	VerifyMetadata func(context.Context, AssetRef, []byte) error
}

// PassBudget is remaining work in one pass, never a lifetime limit. Zero fields
// mean no work remains; NewPassBudget supplies the normal 8/4/30s allowance.
type PassBudget struct {
	Discovery int       `json:"discovery"`
	Evidence  int       `json:"evidence"`
	Deadline  time.Time `json:"deadline"`
}

var ErrPassExhausted = errors.New("release: discovery pass exhausted")

func NewPassBudget() PassBudget {
	return PassBudget{Discovery: 8, Evidence: 4, Deadline: time.Now().Add(30 * time.Second)}
}

func (budget PassBudget) bounded() PassBudget {
	budget.Discovery = min(max(0, budget.Discovery), 8)
	budget.Evidence = min(max(0, budget.Evidence), 4)
	limit := time.Now().Add(30 * time.Second)
	if budget.Deadline.IsZero() || budget.Deadline.After(limit) {
		budget.Deadline = limit
	}
	return budget
}

// ReadDocument charges before transport, and bounds callback execution as well
// as the number and size of documents. Only the unpinned latest root is a hint.
func ReadDocument(ctx context.Context, source DocumentSource, ref AssetRef, kind DocumentKind, budget *PassBudget) ([]byte, error) {
	*budget = budget.bounded()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !time.Now().Before(budget.Deadline) {
		return nil, ErrPassExhausted
	}
	remaining := &budget.Discovery
	if kind == ManifestDocument || kind == ReceiptDocument {
		remaining = &budget.Evidence
	}
	if *remaining == 0 {
		return nil, ErrPassExhausted
	}
	if source.Read == nil {
		return nil, errors.New("release: missing document reader")
	}
	if !(kind == RootDocument && ref.Tag == "latest" && ref.SHA256 == "") {
		if err := ref.Validate(); err != nil {
			return nil, err
		}
	}
	*remaining--
	readCtx, cancel := context.WithDeadline(ctx, budget.Deadline)
	defer cancel()
	data, err := source.Read(readCtx, ref, kind)
	if err != nil {
		return nil, fmt.Errorf("release: read %s %s/%s: %w", kind, ref.Tag, ref.Name, err)
	}
	if !time.Now().Before(budget.Deadline) {
		return nil, ErrPassExhausted
	}
	if len(data) > DocumentLimit(kind) {
		return nil, errors.New("release: document exceeds its byte bound")
	}
	if ref.SHA256 != "" && !matchesDigest(data, ref.SHA256) {
		return nil, errors.New("release: document SHA-256 differs from its reference")
	}
	return data, nil
}

func DocumentLimit(kind DocumentKind) int {
	switch kind {
	case RootDocument:
		return MaxDiscoveryRootBytes
	case MetadataDocument:
		return MaxMetadataBytes
	case PageDocument:
		return MaxAnchorPageBytes
	default:
		return maximumManifest
	}
}

func AuthenticateMetadata(ctx context.Context, source DocumentSource, ref AssetRef, data []byte, epoch uint32) (UpdateMetadata, error) {
	var result UpdateMetadata
	if err := ref.Validate(); err != nil {
		return result, err
	}
	if ref.Name != UpdateMetadataAssetName || len(data) > MaxMetadataBytes || !matchesDigest(data, ref.SHA256) {
		return result, errors.New("release: invalid metadata reference or bytes")
	}
	if source.VerifyMetadata == nil {
		return result, errors.New("release: metadata signature verifier is required")
	}
	if err := source.VerifyMetadata(ctx, ref, data); err != nil {
		return result, fmt.Errorf("release: authenticate metadata: %w", err)
	}
	result, err := DecodeUpdateMetadata(data)
	if err != nil {
		return result, err
	}
	if result.Version != ref.Tag || result.Epoch != epoch {
		return result, errors.New("release: metadata identity or epoch differs from its reference")
	}
	return result, nil
}

type FailedEdge struct {
	FromDigest string   `json:"fromDigest"`
	ToDigest   string   `json:"toDigest"`
	Platform   Platform `json:"platform"`
}

type PageLocation struct {
	Ref  AssetRef `json:"ref"`
	Full bool     `json:"full"`
}

type RouteSearch struct {
	From        Build         `json:"from"`
	At          Position      `json:"at"`
	Low         uint64        `json:"low"`
	High        uint64        `json:"high"`
	Done        bool          `json:"done"`
	Best        *AnchorEntry  `json:"best,omitempty"`
	Pending     *RoutePending `json:"pending,omitempty"`
	DirectTried bool          `json:"directTried"`
}

type RoutePending struct {
	Entry    AnchorEntry     `json:"entry"`
	Direct   bool            `json:"direct"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
	Manifest json.RawMessage `json:"manifest,omitempty"`
}

type RouteCursor struct {
	Epoch  uint32                  `json:"epoch"`
	Target string                  `json:"target"`
	Pages  map[uint64]PageLocation `json:"pages"`
	Search *RouteSearch            `json:"search,omitempty"`
	Failed []FailedEdge            `json:"failed,omitempty"`
}

// PlannedRelease proves a forward edge only. Automatic installation additionally
// requires signature and rollback authorization from the host updater.
type PlannedRelease struct {
	Metadata   *UpdateMetadata   `json:"metadata,omitempty"`
	Position   Position          `json:"position"`
	Manifest   Manifest          `json:"manifest"`
	Transition Transition        `json:"transition"`
	Receipt    TransitionReceipt `json:"receipt"`
}

type RouteStep struct {
	Cursor    RouteCursor     `json:"cursor"`
	Next      *PlannedRelease `json:"next,omitempty"`
	Budget    PassBudget      `json:"budget"`
	Exhausted bool            `json:"exhausted"`
	Waiting   bool            `json:"waiting"`
}

// NextHop searches authenticated index entries, then verifies the exact manifest
// transition and receipt. target must come from AuthenticateMetadata. No manifest
// from a newer epoch is read here. The cursor can be serialized between passes.
func NextHop(ctx context.Context, from Build, at Position, target UpdateMetadata, cursor RouteCursor, read DocumentSource, budget PassBudget) (RouteStep, error) {
	step := RouteStep{Cursor: cursor, Budget: budget.bounded()}
	if err := target.Validate(); err != nil {
		return step, err
	}
	if err := validateRouteSource(from, at, target); err != nil {
		return step, err
	}
	step.Cursor = cloneRouteCursor(cursor)
	prepareRoute(&step.Cursor, from, at, target)
	passCtx, cancel := context.WithDeadline(ctx, step.Budget.Deadline)
	defer cancel()
	err := runRoute(passCtx, from, at, target, &step, read)
	if errors.Is(err, ErrPassExhausted) {
		step.Exhausted = true
		return step, nil
	}
	return step, err
}

func validateRouteSource(from Build, at Position, target UpdateMetadata) error {
	if err := validateMetadataVersion(from.Version); err != nil {
		return err
	}
	if from.Modified || !validDigest(from.Digest) || !supportedPlatform(from.Platform) || from.StateVersion <= 0 || from.WorkerProtocol <= 0 {
		return errors.New("release: route requires an exact unmodified source build")
	}
	order, _ := CompareVersions(from.Version, target.Version)
	if order >= 0 || at.Ordinal > target.Position.Ordinal {
		return errors.New("release: route target must follow the source")
	}
	return nil
}

func cloneRouteCursor(cursor RouteCursor) RouteCursor {
	pages := make(map[uint64]PageLocation, len(cursor.Pages))
	for number, page := range cursor.Pages {
		pages[number] = page
	}
	cursor.Pages = pages
	if cursor.Search != nil {
		copySearch := *cursor.Search
		cursor.Search = &copySearch
	}
	if cursor.Search != nil && cursor.Search.Pending != nil {
		pending := *cursor.Search.Pending
		cursor.Search.Pending = &pending
	}
	return cursor
}

func prepareRoute(cursor *RouteCursor, from Build, at Position, target UpdateMetadata) {
	data, _ := json.Marshal(target)
	token := digestBytes(data)
	changed := cursor.Epoch != target.Epoch || cursor.Target != token
	if changed {
		retainFullPages(cursor, target.Epoch)
	}
	if changed || cursor.Search == nil || cursor.Search.From != from || cursor.Search.At != at {
		cursor.Search = newRouteSearch(from, at, target)
	}
	cursor.Epoch, cursor.Target = target.Epoch, token
	last := target.Position.Ordinal
	if target.Position.Anchor && last > 0 {
		last--
	}
	full := !(target.Position.Anchor && target.Position.Ordinal == 0) && last%AnchorsPerPage == AnchorsPerPage-1
	cursor.Pages[last/AnchorsPerPage] = PageLocation{Ref: target.Page, Full: full}
	trimPageLocations(cursor.Pages)
}

func retainFullPages(cursor *RouteCursor, epoch uint32) {
	for number, page := range cursor.Pages {
		if !page.Full || cursor.Epoch != epoch {
			delete(cursor.Pages, number)
		}
	}
}

func newRouteSearch(from Build, at Position, target UpdateMetadata) *RouteSearch {
	search := &RouteSearch{From: from, At: at, Low: at.Ordinal + 1, High: target.Position.Ordinal}
	if target.Position.Anchor && search.High > 0 {
		search.High--
	}
	search.Done = at.Ordinal == ^uint64(0) || search.Low > search.High || target.Position.Anchor && target.Position.Ordinal == 0
	if !at.Anchor {
		search.High = search.Low
	}
	return search
}

func runRoute(ctx context.Context, from Build, at Position, target UpdateMetadata, step *RouteStep, read DocumentSource) error {
	search := step.Cursor.Search
	if !search.DirectTried && directSource(from, at, target) {
		search.Pending = &RoutePending{Direct: true}
	}
	search.DirectTried = true
	for {
		if search.Pending != nil {
			if err := finishRouteCandidate(ctx, from, target, step, read); err != nil {
				return err
			}
			if step.Next != nil {
				return nil
			}
		}
		if !search.Done {
			if err := searchRouteEntry(ctx, at, target, step, read); err != nil {
				return err
			}
			continue
		}
		if search.Best == nil {
			step.Waiting = true
			return nil
		}
		search.Pending = &RoutePending{Entry: *search.Best}
		search.Best = nil
	}
}

func directSource(from Build, at Position, target UpdateMetadata) bool {
	return at.Anchor && at.Ordinal >= target.Direct.AnchorsFrom || slices.Contains(target.Direct.Since, from.Version)
}

func searchRouteEntry(ctx context.Context, at Position, target UpdateMetadata, step *RouteStep, read DocumentSource) error {
	search := step.Cursor.Search
	middle := search.Low + (search.High-search.Low)/2
	entry, err := routeEntry(ctx, middle, target, step, read)
	if err != nil {
		return err
	}
	if entry.AnchorsFrom <= at.Ordinal {
		search.Best = &entry
		search.Done = middle == search.High
		search.Low = middle + 1
		return nil
	}
	search.Done = middle == search.Low
	search.High = middle - 1
	return nil
}

func routeEntry(ctx context.Context, ordinal uint64, target UpdateMetadata, step *RouteStep, read DocumentSource) (AnchorEntry, error) {
	wanted := ordinal / AnchorsPerPage
	for {
		number, location, ok := nearestPage(step.Cursor.Pages, wanted)
		if !ok {
			return AnchorEntry{}, errors.New("release: no authenticated page path")
		}
		data, err := ReadDocument(ctx, read, location.Ref, PageDocument, &step.Budget)
		if err != nil {
			return AnchorEntry{}, err
		}
		page, err := DecodeAnchorPage(data, location.Ref)
		if err != nil {
			return AnchorEntry{}, err
		}
		if page.Page != number || page.Epoch != target.Epoch {
			return AnchorEntry{}, errors.New("release: anchor page identity differs from its reference")
		}
		learnPages(&step.Cursor, page, location.Ref)
		if number != wanted {
			continue
		}
		index := ordinal % AnchorsPerPage
		if index >= uint64(len(page.Entries)) {
			return AnchorEntry{}, errors.New("release: anchor ordinal absent from page")
		}
		return page.Entries[index], nil
	}
}

func nearestPage(pages map[uint64]PageLocation, wanted uint64) (uint64, PageLocation, bool) {
	var best uint64
	var location PageLocation
	found := false
	for number, candidate := range pages {
		if number < wanted || found && number >= best {
			continue
		}
		best, location, found = number, candidate, true
	}
	return best, location, found
}

func learnPages(cursor *RouteCursor, page AnchorPage, ref AssetRef) {
	cursor.Pages[page.Page] = PageLocation{Ref: ref, Full: len(page.Entries) == AnchorsPerPage}
	for _, previous := range page.Back {
		cursor.Pages[previous.Page] = PageLocation{Ref: previous.Ref, Full: true}
	}
	trimPageLocations(cursor.Pages)
}

func trimPageLocations(pages map[uint64]PageLocation) {
	if len(pages) <= 256 {
		return
	}
	numbers := make([]uint64, 0, len(pages))
	for number := range pages {
		numbers = append(numbers, number)
	}
	slices.Sort(numbers)
	for _, number := range numbers[:len(numbers)-256] {
		delete(pages, number)
	}
}

func finishRouteCandidate(ctx context.Context, from Build, target UpdateMetadata, step *RouteStep, read DocumentSource) error {
	pending := step.Cursor.Search.Pending
	metadata, manifestRef, err := candidateMetadata(ctx, target, pending, read, &step.Budget)
	if err != nil {
		return err
	}
	if len(pending.Manifest) == 0 {
		pending.Manifest, err = ReadDocument(ctx, read, manifestRef, ManifestDocument, &step.Budget)
		if err != nil {
			return err
		}
	}
	if !matchesDigest(pending.Manifest, manifestRef.SHA256) {
		return errors.New("release: pending manifest SHA-256 differs")
	}
	manifest, err := decodeManifest(pending.Manifest)
	if err != nil {
		return err
	}
	if manifest.Version != manifestRef.Tag {
		return errors.New("release: manifest identity differs from route candidate")
	}
	if err := manifest.Allows(from); err != nil {
		return err
	}
	transition, err := exactRouteTransition(from, manifest)
	if err != nil {
		return err
	}
	edge := FailedEdge{FromDigest: from.Digest, ToDigest: transition.ToDigest, Platform: from.Platform}
	if slices.Contains(step.Cursor.Failed, edge) {
		excludeRouteCandidate(step.Cursor.Search, pending)
		return nil
	}
	ref := AssetRef{Tag: manifest.Version, Name: transition.Proof + ".json", SHA256: transition.Proof}
	data, err := ReadDocument(ctx, read, ref, ReceiptDocument, &step.Budget)
	if err != nil {
		return err
	}
	receipt, err := VerifyTransitionReceipt(data, transition, manifest.Compatibility)
	if err != nil {
		return err
	}
	if from.StateVersion < receipt.StateReadMin || from.StateVersion > receipt.StateReadMax || from.WorkerProtocol < receipt.WorkerMin || from.WorkerProtocol > receipt.WorkerMax {
		return errors.New("release: source capabilities are outside the exact receipt's tested range")
	}
	position := Position{Anchor: true, Ordinal: pending.Entry.Ordinal}
	if metadata != nil {
		position = metadata.Position
	}
	step.Next = &PlannedRelease{Metadata: metadata, Position: position, Manifest: manifest, Transition: transition, Receipt: receipt}
	return nil
}

func candidateMetadata(ctx context.Context, target UpdateMetadata, pending *RoutePending, read DocumentSource, budget *PassBudget) (*UpdateMetadata, AssetRef, error) {
	if pending.Direct {
		return &target, AssetRef{Tag: target.Version, Name: ManifestAssetName, SHA256: target.Manifest}, nil
	}
	entry := pending.Entry
	if entry.Metadata == "" {
		return nil, AssetRef{Tag: entry.Version, Name: ManifestAssetName, SHA256: entry.Manifest}, nil
	}
	ref := AssetRef{Tag: entry.Version, Name: UpdateMetadataAssetName, SHA256: entry.Metadata}
	var err error
	if len(pending.Metadata) == 0 {
		pending.Metadata, err = ReadDocument(ctx, read, ref, MetadataDocument, budget)
	}
	if err != nil {
		return nil, AssetRef{}, err
	}
	metadata, err := AuthenticateMetadata(ctx, read, ref, pending.Metadata, target.Epoch)
	if err != nil {
		return nil, AssetRef{}, err
	}
	if !metadata.Position.Anchor || metadata.Position.Ordinal != entry.Ordinal || metadata.Direct.AnchorsFrom != entry.AnchorsFrom || !metadata.PublishedAt.Equal(entry.PublishedAt) {
		return nil, AssetRef{}, errors.New("release: anchor metadata differs from index entry")
	}
	return &metadata, AssetRef{Tag: metadata.Version, Name: ManifestAssetName, SHA256: metadata.Manifest}, nil
}

func exactRouteTransition(from Build, manifest Manifest) (Transition, error) {
	artifact, err := manifest.Artifact(from.Platform)
	if err != nil {
		return Transition{}, err
	}
	for _, transition := range manifest.Compatibility.Transitions {
		if transition.Platform == from.Platform && transition.FromDigest == from.Digest && transition.ToDigest == artifact.BinarySHA256 {
			return transition, nil
		}
	}
	return Transition{}, errors.New("release: exact route transition is absent")
}

func excludeRouteCandidate(search *RouteSearch, pending *RoutePending) {
	search.Pending = nil
	if pending.Direct {
		return
	}
	if pending.Entry.Ordinal <= search.At.Ordinal+1 {
		search.Done = true
		return
	}
	search.Low, search.High = search.At.Ordinal+1, pending.Entry.Ordinal-1
	search.Done = false
	if !search.At.Anchor {
		search.Done = true
	}
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
