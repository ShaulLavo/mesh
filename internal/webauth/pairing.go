package webauth

import (
	"context"
	"crypto/rand"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
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

func (s *Service) Begin(ctx context.Context, w http.ResponseWriter, r *http.Request) (Pairing, error) {
	token, err := bearer()
	if err != nil {
		return Pairing{}, err
	}
	code, err := approvalCode()
	if err != nil {
		return Pairing{}, err
	}
	var pairing Pairing
	err = s.change(ctx, func(d *state, now time.Time) error {
		if len(d.Pairs) >= maxPairs {
			return ErrCapacity
		}
		old, _ := browserIn(d, r, now)
		p := pairRecord{CodeHash: codeHash(code), ExpiresAt: now.Add(pairTTL), ExistingID: old.ID}
		if existing, err := cookieKey(r, PairCookie); err == nil {
			delete(d.Pairs, existing)
		}
		d.Pairs[hash(token)] = p
		pairing = Pairing{Code: code, ExpiresAt: p.ExpiresAt}
		return nil
	})
	if err != nil {
		return Pairing{}, err
	}
	writeCookie(w, PairCookie, token, pairing.ExpiresAt, s.now())
	return pairing, nil
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
