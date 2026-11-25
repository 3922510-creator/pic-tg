package storage

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"path"
)

// PublicToken persists a random alias independently of replace-style object writes.
// A join on read makes retained aliases unusable after an object is deleted.
func (d *DB) PublicToken(key string) (string, error) {
	var token string
	err := d.QueryRow("SELECT token FROM public_links WHERE object_key = ?", key).Scan(&token)
	if err == nil {
		return token, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	token = hex.EncodeToString(random[:]) + path.Ext(key)
	_, err = d.Exec(`INSERT INTO public_links (object_key, token)
 SELECT key, ? FROM objects WHERE key = ? ON CONFLICT(object_key) DO NOTHING`, token, key)
	if err != nil {
		return "", err
	}
	err = d.QueryRow("SELECT token FROM public_links WHERE object_key = ?", key).Scan(&token)
	return token, err
}

func (d *DB) KeyForPublicToken(token string) (string, error) {
	var key string
	err := d.QueryRow(`SELECT o.key FROM objects o JOIN public_links p ON p.object_key = o.key WHERE p.token = ?`, token).Scan(&key)
	return key, err
}
