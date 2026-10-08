package app

import (
	"context"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/minisign"
	"github.com/corticoide/mockvision/backend/internal/pkg"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
)

// TrustedKeyView is a key whose package signatures the node trusts: an
// official catalog key built into the binary, or one an admin added.
type TrustedKeyView struct {
	ID        string     `json:"id,omitempty"`
	Name      string     `json:"name"`
	KeyID     string     `json:"key_id"`
	PublicKey string     `json:"public_key"`
	Builtin   bool       `json:"builtin"`
	AddedAt   *time.Time `json:"added_at,omitempty"`
}

// TrustedKeyInput adds a key.
type TrustedKeyInput struct {
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
}

// maxKeyName bounds a trusted key's name.
const maxKeyName = 64

// ListTrustedKeys returns the official keys, then the added ones.
func (s *Service) ListTrustedKeys(ctx context.Context) ([]TrustedKeyView, error) {
	out := []TrustedKeyView{}
	for _, k := range pkg.OfficialKeys() {
		pub, _ := minisign.ParsePublicKey(k.Key)
		out = append(out, TrustedKeyView{Name: k.Name, KeyID: minisign.KeyID(pub.ID), PublicKey: k.Key, Builtin: true})
	}
	rows, err := s.store.R().ListTrustedKeys(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		at := store.Time(r.AddedAt)
		out = append(out, TrustedKeyView{ID: r.ID, Name: r.Name, KeyID: r.KeyID, PublicKey: r.PublicKey, AddedAt: &at})
	}
	return out, nil
}

// AddTrustedKey trusts the signatures of a minisign public key from now
// on; packages imported before keep the status they got then.
func (s *Service) AddTrustedKey(ctx context.Context, actor Actor, in TrustedKeyInput) (*TrustedKeyView, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > maxKeyName {
		return nil, domain.Invalid("name", "a key needs a name of up to %d characters", maxKeyName)
	}
	pub, err := minisign.ParsePublicKey(in.PublicKey)
	if err != nil {
		return nil, domain.Invalid("public_key", "not a minisign public key: paste its .pub file or its base64 line")
	}
	keyID := minisign.KeyID(pub.ID)
	for _, k := range pkg.OfficialKeys() {
		if o, _ := minisign.ParsePublicKey(k.Key); o.ID == pub.ID {
			return nil, domain.Conflict("public_key", "key %s is an official catalog key, trusted already", keyID)
		}
	}
	now := time.Now()
	row := db.InsertTrustedKeyParams{ID: ulid.Make().String(), Name: name, KeyID: keyID, PublicKey: pub.String(), AddedAt: now.UnixMilli()}
	if err := s.store.W().InsertTrustedKey(ctx, row); err != nil {
		if store.IsUnique(err) {
			return nil, domain.Conflict("public_key", "key %s is trusted already", keyID)
		}
		return nil, err
	}
	s.audit(ctx, actor, "trusted_key.add", "trusted_key", row.ID, map[string]any{"name": name, "key_id": keyID})
	return &TrustedKeyView{ID: row.ID, Name: name, KeyID: keyID, PublicKey: row.PublicKey, AddedAt: &now}, nil
}

// DeleteTrustedKey stops trusting a key. Official keys cannot be removed.
func (s *Service) DeleteTrustedKey(ctx context.Context, actor Actor, id string) error {
	k, err := s.store.R().GetTrustedKey(ctx, id)
	if err != nil {
		return store.NotFound(err)
	}
	if _, err := s.store.W().DeleteTrustedKey(ctx, id); err != nil {
		return err
	}
	s.audit(ctx, actor, "trusted_key.delete", "trusted_key", id, map[string]any{"name": k.Name, "key_id": k.KeyID})
	return nil
}

// inspectOptions are the keys the import pipeline trusts now.
func (s *Service) inspectOptions(ctx context.Context) (pkg.Options, error) {
	opts := pkg.Options{Keys: pkg.OfficialKeys()}
	rows, err := s.store.R().ListTrustedKeys(ctx)
	if err != nil {
		return opts, err
	}
	for _, r := range rows {
		opts.Keys = append(opts.Keys, pkg.TrustedKey{Name: r.Name, Key: r.PublicKey})
	}
	return opts, nil
}
