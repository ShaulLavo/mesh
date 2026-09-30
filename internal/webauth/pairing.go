package webauth

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
)

func approvalCode() (string, error) {
	const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"
	var random [10]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	for i := range random {
		random[i] = alphabet[int(random[i])%len(alphabet)]
	}
	return string(random[:5]) + "-" + string(random[5:]), nil
}
func codeHash(code string) string {
	return hash(strings.ToLower(strings.ReplaceAll(strings.TrimSpace(code), "-", "")))
}

func (s *Service) Begin(ctx context.Context, w http.ResponseWriter, r *http.Request, ip netip.Addr) (Pairing, error) {
	if err := ctx.Err(); err != nil {
		return Pairing{}, fmt.Errorf("pending browser pairing: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	s.state.prune(now)
	if p, err := s.pending(r, now); err == nil {
		return p, nil
	}
	ip = ip.Unmap()
	if !ip.IsValid() {
		return Pairing{}, ErrPairing
	}
	source := ip.String()
	if ip.Is6() {
		source = netip.PrefixFrom(ip, 64).Masked().String()
	}
	oldest, window, err := s.pairingSlot(source, now)
	if err != nil {
		return Pairing{}, err
	}
	token, err := bearer()
	if err != nil {
		return Pairing{}, err
	}
	code, err := approvalCode()
	if err != nil {
		return Pairing{}, err
	}
	if oldest != "" {
		delete(s.state.Pairs, oldest)
	}
	old, _ := browserIn(&s.state, r, now)
	p := pairRecord{Code: code, Source: source, CodeHash: codeHash(code), ExpiresAt: now.Add(pairTTL), ExistingID: old.ID,
		PairingInfo: PairingInfo{CreatedAt: now, UserAgent: userAgentSummary(r.UserAgent()), SourceIP: ip.String()}}
	s.state.Pairs[hash(token)] = p
	window.Issued++
	s.sources[source] = window
	writeCookie(w, PairCookie, token, p.ExpiresAt, now)
	return Pairing{Code: code, ExpiresAt: p.ExpiresAt}, nil
}

func (s *Service) pairingSlot(source string, now time.Time) (string, sourceWindow, error) {
	for key, window := range s.sources {
		if !now.Before(window.StartedAt.Add(time.Minute)) {
			delete(s.sources, key)
		}
	}
	window, tracked := s.sources[source]
	if window.Issued >= maxSourceStarts {
		return "", sourceWindow{}, ErrRateLimited
	}
	if !tracked && len(s.sources) >= maxPairSources {
		return "", sourceWindow{}, ErrCapacity
	}
	count, oldest := s.sourcePairs(source)
	if count >= maxSourcePairs && oldest == "" {
		return "", sourceWindow{}, ErrCapacity
	}
	if count < maxSourcePairs && len(s.state.Pairs) >= maxPairs {
		return "", sourceWindow{}, ErrCapacity
	}
	if !tracked {
		window.StartedAt = now
	}
	if count < maxSourcePairs {
		oldest = ""
	}
	return oldest, window, nil
}

func (s *Service) sourcePairs(source string) (int, string) {
	count, oldest := 0, ""
	for key, pair := range s.state.Pairs {
		if pair.Source != source {
			continue
		}
		count++
		if pair.Owner == "" && (oldest == "" || pair.CreatedAt.Before(s.state.Pairs[oldest].CreatedAt)) {
			oldest = key
		}
	}
	return count, oldest
}

func userAgentSummary(value string) string {
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return -1
		}
		return r
	}, value)
	clean = strings.Join(strings.Fields(clean), " ")
	if len([]rune(clean)) > 256 {
		clean = string([]rune(clean)[:256])
	}
	if clean == "" {
		return "Unknown browser"
	}
	return clean
}

func (s *Service) pending(r *http.Request, now time.Time) (Pairing, error) {
	key, err := cookieKey(r, PairCookie)
	if err != nil {
		return Pairing{}, ErrPairing
	}
	p, ok := s.state.Pairs[key]
	if !ok || !now.Before(p.ExpiresAt) || p.Code == "" {
		return Pairing{}, ErrPairing
	}
	return Pairing{Code: p.Code, ExpiresAt: p.ExpiresAt}, nil
}

func (s *Service) Pending(ctx context.Context, r *http.Request) (Pairing, error) {
	if err := ctx.Err(); err != nil {
		return Pairing{}, fmt.Errorf("pending browser pairing: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending(r, s.now())
}

func (s *Service) Inspect(ctx context.Context, code string) (PairingInfo, error) {
	if err := ctx.Err(); err != nil {
		return PairingInfo{}, fmt.Errorf("inspect browser pairing: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, p, ok := findPair(&s.state, codeHash(code))
	if !ok || p.Owner != "" || !s.now().Before(p.ExpiresAt) {
		return PairingInfo{}, ErrPairing
	}
	info := p.PairingInfo
	info.AgeSeconds = max(0, int64(s.now().Sub(p.CreatedAt).Seconds()))
	return info, nil
}

func (s *Service) Approve(ctx context.Context, code, owner string) error {
	if !validOwner(owner) {
		return ErrUnauthorized
	}
	return s.change(ctx, func(d *state, now time.Time) error {
		if !now.Before(d.ApprovalWindow.Add(time.Minute)) {
			d.ApprovalWindow = now
			d.ApprovalAttempts = 0
		}
		if d.ApprovalAttempts >= 30 {
			return ErrRateLimited
		}
		d.ApprovalAttempts++
		key, p, ok := findPair(d, codeHash(code))
		if !ok || p.Owner != "" {
			return ErrPairing
		}
		p.Owner = owner
		d.Pairs[key] = p
		return nil
	})
}
func findPair(d *state, codeHash string) (string, pairRecord, bool) {
	for key, p := range d.Pairs {
		if p.CodeHash == codeHash {
			return key, p, true
		}
	}
	return "", pairRecord{}, false
}

func (s *Service) Promote(ctx context.Context, w http.ResponseWriter, r *http.Request) (Session, error) {
	if err := s.checkPairApproval(ctx, r); err != nil {
		return Session{}, err
	}
	token, err := bearer()
	if err != nil {
		return Session{}, err
	}
	id, err := bearer()
	if err != nil {
		return Session{}, err
	}
	csrf, err := bearer()
	if err != nil {
		return Session{}, err
	}
	var result Session
	err = s.change(ctx, func(d *state, now time.Time) error {
		pairKey, err := cookieKey(r, PairCookie)
		if err != nil {
			return ErrPairing
		}
		p, ok := d.Pairs[pairKey]
		if !ok {
			return ErrPairing
		}
		if p.Owner == "" {
			return ErrApprovalPending
		}
		b := browserRecord{Session: Session{ID: id, CSRF: csrf, ExpiresAt: now.Add(sessionTTL)}, CreatedAt: now}
		b, err = promotionBrowser(d, r, p, b, now)
		if err != nil {
			return err
		}
		if p.ExistingID == "" && len(d.Browsers) >= maxBrowsers {
			return ErrCapacity
		}
		if len(b.Owners) >= maxOwners && !b.Owns(p.Owner) {
			return ErrCapacity
		}
		b.Owners = appendOwner(b.Owners, p.Owner)
		removeBrowserToken(d, b.ID)
		d.Browsers[hash(token)] = b
		delete(d.Pairs, pairKey)
		result = b.Session
		result.Owners = slices.Clone(result.Owners)
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	writeCookie(w, OwnerCookie, token, result.ExpiresAt, s.now())
	http.SetCookie(w, &http.Cookie{Name: PairCookie, Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	return result, nil
}
func (s *Service) checkPairApproval(ctx context.Context, r *http.Request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key, err := cookieKey(r, PairCookie)
	if err != nil {
		return ErrPairing
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pair, ok := s.state.Pairs[key]
	if !ok || !s.now().Before(pair.ExpiresAt) {
		return ErrPairing
	}
	if pair.Owner == "" {
		return ErrApprovalPending
	}
	return nil
}
func appendOwner(owners []string, owner string) []string {
	if slices.Contains(owners, owner) {
		return owners
	}
	return append(owners, owner)
}
func removeBrowserToken(d *state, id string) {
	for key, b := range d.Browsers {
		if b.ID == id {
			delete(d.Browsers, key)
		}
	}
}

func (s *Service) Browser(ctx context.Context, r *http.Request) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := browserIn(&s.state, r, s.now())
	return b.Session, err
}
func (s *Service) List(ctx context.Context, owner string) ([]BrowserInfo, error) {
	if !validOwner(owner) {
		return nil, ErrUnauthorized
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := []BrowserInfo{}
	for _, b := range s.state.Browsers {
		if !s.now().Before(b.ExpiresAt) || !b.Owns(owner) {
			continue
		}
		result = append(result, BrowserInfo{ID: b.ID, CreatedAt: b.CreatedAt, ExpiresAt: b.ExpiresAt})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}
func (s *Service) Revoke(ctx context.Context, owner, id string) error {
	if !validOwner(owner) {
		return ErrUnauthorized
	}
	return s.change(ctx, func(d *state, now time.Time) error {
		b, err := browserByID(d, id, now)
		if err != nil || !b.Owns(owner) {
			return ErrUnauthorized
		}
		removeOwner(d, id, owner)
		removeViews(d.Tickets, id, owner)
		removeViews(d.Views, id, owner)
		removePending(d, id, owner)
		return nil
	})
}
func removeOwner(d *state, id, owner string) {
	for key, b := range d.Browsers {
		if b.ID != id {
			continue
		}
		b.Owners = slices.DeleteFunc(b.Owners, func(o string) bool { return o == owner })
		if len(b.Owners) == 0 {
			delete(d.Browsers, key)
			continue
		}
		d.Browsers[key] = b
	}
}
func removeViews(records map[string]viewRecord, id, owner string) {
	for key, v := range records {
		if v.BrowserID == id && v.Owner == owner {
			delete(records, key)
		}
	}
}
func removePending(d *state, id, owner string) {
	for key, p := range d.Pairs {
		if p.ExistingID == id && (p.Owner == owner || p.Owner == "") {
			delete(d.Pairs, key)
		}
	}
}

func promotionBrowser(d *state, r *http.Request, p pairRecord, fresh browserRecord, now time.Time) (browserRecord, error) {
	if p.ExistingID == "" {
		return fresh, nil
	}
	existing, err := browserIn(d, r, now)
	if err != nil || existing.ID != p.ExistingID {
		return browserRecord{}, ErrUnauthorized
	}
	return existing, nil
}
