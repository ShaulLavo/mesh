package wake

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"math"
	"os"
	"path/filepath"
	"time"
)

type AuthorityOptions struct {
	Discover func(context.Context) (NIC, error)
	Now      func() time.Time
}

type Authority struct {
	path    string
	key     ed25519.PrivateKey
	options AuthorityOptions
}

type policy struct {
	Allowed bool  `json:"allowed"`
	Grant   Grant `json:"grant"`
}

func NewAuthority(stateDir string, key ed25519.PrivateKey) (*Authority, error) {
	return NewAuthorityWithOptions(stateDir, key, AuthorityOptions{})
}

func NewAuthorityWithOptions(stateDir string, key ed25519.PrivateKey, options AuthorityOptions) (*Authority, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("wake authority requires an Ed25519 private key")
	}
	dir, err := wakeDir(stateDir)
	if err != nil {
		return nil, err
	}
	if options.Discover == nil {
		options.Discover = Discover
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Authority{path: filepath.Join(dir, "policy.json"), key: append(ed25519.PrivateKey(nil), key...), options: options}, nil
}

func (a *Authority) SetAllowed(ctx context.Context, allowed bool) (Grant, error) {
	return a.current(ctx, &allowed)
}
func (a *Authority) Current(ctx context.Context) (Grant, error) { return a.current(ctx, nil) }

func (a *Authority) current(ctx context.Context, allowed *bool) (Grant, error) {
	if err := ctx.Err(); err != nil {
		return Grant{}, err
	}
	lock, err := lockFile(ctx, a.path+".lock")
	if err != nil {
		return Grant{}, err
	}
	defer func() { _ = lock.Close() }()
	prior, err := a.load()
	if err != nil {
		return Grant{}, err
	}
	next := prior
	if allowed != nil {
		next.Allowed = *allowed
	}
	nic, discoveryErr := a.discover(ctx, next.Allowed)
	if err := ctx.Err(); err != nil {
		return Grant{}, err
	}
	now := a.options.Now().UTC()
	if discoveryErr != nil && !errors.Is(discoveryErr, ErrUnsupportedNIC) && prior.Allowed && next.Allowed && prior.Grant.Enabled && now.Before(prior.Grant.ExpiresAt) {
		return prior.Grant, nil
	}
	next.Grant = Grant{TargetID: base64.RawURLEncoding.EncodeToString(a.key.Public().(ed25519.PublicKey)), Enabled: nic != nil, Revision: prior.Grant.Revision, NIC: nic}
	changed := next.Allowed != prior.Allowed || !samePolicy(prior.Grant, next.Grant)
	if !changed && prior.Grant.ExpiresAt.After(now.Add(GrantLifetime/2)) {
		return prior.Grant, nil
	}
	if changed || next.Grant.Revision == 0 {
		if next.Grant.Revision >= math.MaxInt64 {
			return Grant{}, errors.New("wake policy revision exhausted")
		}
		next.Grant.Revision++
	}
	next.Grant.IssuedAt = now
	next.Grant.ExpiresAt = now.Add(GrantLifetime)
	next.Grant.Signature = ed25519.Sign(a.key, grantTranscript(next.Grant))
	if err := writeJSON(a.path, next); err != nil {
		return Grant{}, err
	}
	return next.Grant, nil
}

func (a *Authority) load() (policy, error) {
	var value policy
	err := readJSON(a.path, &value)
	if errors.Is(err, os.ErrNotExist) {
		return policy{}, nil
	}
	if err != nil {
		return policy{}, err
	}
	if value.Grant.TargetID != base64.RawURLEncoding.EncodeToString(a.key.Public().(ed25519.PublicKey)) {
		return policy{}, errors.New("wake policy belongs to another identity")
	}
	if err := validateSignedGrant(value.Grant); err != nil {
		return policy{}, err
	}
	return value, nil
}

func (a *Authority) discover(ctx context.Context, allowed bool) (*NIC, error) {
	if !allowed {
		return nil, nil
	}
	nic, err := a.options.Discover(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateNIC(nic); err != nil {
		return nil, errors.Join(ErrUnsupportedNIC, err)
	}
	return &nic, nil
}
