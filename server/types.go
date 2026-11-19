package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const pluginID = "space.altlinux.bugzilla"
const serverTTL = 90 * time.Second
const clientTTL = 5 * time.Minute

type config struct {
	BugzillaURL       string
	APIKeyTransport   string
	EncryptionKeyFile string
}
type connection struct {
	Revision   string `json:"revision"`
	Ciphertext string `json:"ciphertext,omitempty"`
	AccountID  int    `json:"account_id,omitempty"`
	Login      string `json:"login,omitempty"`
}
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string                    { return e.Message }
func failure(status int, code, message string) error { return &apiError{status, code, message} }

var errChanged = &apiError{409, "connection_changed", "Подключение изменилось. Повторите запрос."}

type bug struct {
	ID         int    `json:"id"`
	Summary    string `json:"summary"`
	Status     string `json:"status"`
	Resolution string `json:"resolution"`
	Priority   string `json:"priority"`
	Assignee   string `json:"assigned_to"`
	Product    string `json:"product"`
	Component  string `json:"component"`
	Updated    string `json:"last_change_time"`
}
type comment struct {
	ID      int    `json:"id"`
	Count   int    `json:"count"`
	Text    string `json:"text"`
	Creator string `json:"creator"`
	Created string `json:"creation_time"`
	Private bool   `json:"is_private"`
}
type card struct {
	Bug             bug       `json:"bug"`
	Comment         *comment  `json:"comment,omitempty"`
	Description     string    `json:"description,omitempty"`
	URL             string    `json:"url"`
	Mode            string    `json:"mode"`
	Revision        string    `json:"revision"`
	SourceFetchedAt time.Time `json:"source_fetched_at"`
	MaxAgeMS        int64     `json:"max_age_ms"`
}

type recordStore interface {
	Get(string) ([]byte, error)
	CAS(string, []byte, []byte) (bool, error)
}

func configIdentity(c config) string {
	d := sha256.Sum256([]byte(c.BugzillaURL + "|" + c.APIKeyTransport))
	return hex.EncodeToString(d[:12])
}
func connectionKey(c config, user string) string {
	d := sha256.Sum256([]byte(user + "|" + configIdentity(c)))
	return "connection_" + hex.EncodeToString(d[:])
}
func loadConnection(store recordStore, c config, user string) (connection, error) {
	b, err := store.Get(connectionKey(c, user))
	if err != nil {
		return connection{}, err
	}
	if len(b) == 0 {
		return connection{Revision: "public"}, nil
	}
	var v connection
	if json.Unmarshal(b, &v) != nil || v.Revision == "" {
		return connection{}, errors.New("invalid connection record")
	}
	return v, nil
}
func storeConnection(store recordStore, c config, user string, v connection) error {
	key := connectionKey(c, user)
	for i := 0; i < 5; i++ {
		old, err := store.Get(key)
		if err != nil {
			return err
		}
		v.Revision = randomID()
		next, _ := json.Marshal(v)
		ok, err := store.CAS(key, old, next)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return errors.New("connection update conflict")
}
func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func encrypt(key []byte, value, aad string) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(g.Seal(nonce, nonce, []byte(value), []byte(aad))), nil
}
func decrypt(key []byte, value, aad string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(b) < g.NonceSize() {
		return "", errors.New("invalid ciphertext")
	}
	plain, err := g.Open(nil, b[:g.NonceSize()], b[g.NonceSize():], []byte(aad))
	return string(plain), err
}
func normalizeConfig(c config) (config, error) {
	c.BugzillaURL = strings.TrimRight(strings.TrimSpace(c.BugzillaURL), "/")
	u, err := url.Parse(c.BugzillaURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return c, errors.New("BugzillaURL must be an HTTPS server URL without credentials, query or fragment")
	}
	if c.APIKeyTransport == "" {
		c.APIKeyTransport = "query"
	}
	if c.APIKeyTransport != "query" && c.APIKeyTransport != "header" {
		return c, errors.New("invalid APIKeyTransport")
	}
	return c, nil
}
func parseReference(raw, base string) (int, *int, error) {
	u, e := url.Parse(raw)
	if e != nil {
		return 0, nil, errors.New("invalid URL")
	}
	b, e := url.Parse(base)
	if e != nil {
		return 0, nil, e
	}
	if u.Scheme != b.Scheme || !strings.EqualFold(u.Host, b.Host) || u.User != nil {
		return 0, nil, errors.New("unsupported URL")
	}
	prefix := strings.TrimRight(b.Path, "/")
	if !strings.HasPrefix(u.Path, prefix+"/") {
		return 0, nil, errors.New("unsupported path")
	}
	p := strings.TrimPrefix(u.Path, prefix)
	var rawID string
	if p == "/show_bug.cgi" {
		if len(u.Query()["id"]) != 1 {
			return 0, nil, errors.New("invalid ID")
		}
		rawID = u.Query().Get("id")
	} else {
		rawID = strings.TrimPrefix(p, "/")
	}
	id, e := strconv.Atoi(rawID)
	if e != nil || id <= 0 || strconv.Itoa(id) != rawID {
		return 0, nil, errors.New("invalid ID")
	}
	var count *int
	if u.Fragment != "" {
		if !strings.HasPrefix(u.Fragment, "c") {
			return id, nil, nil
		}
		v, e := strconv.Atoi(strings.TrimPrefix(u.Fragment, "c"))
		if e != nil || v < 0 {
			return 0, nil, errors.New("invalid comment")
		}
		count = &v
	}
	return id, count, nil
}
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
func cacheKey(user string, c config, rev string, id int, count *int) string {
	suffix := "bug"
	if count != nil {
		suffix = fmt.Sprintf("comment:%d", *count)
	}
	return fmt.Sprintf("v1|%s|%s|%s|%d|%s", user, configIdentity(c), rev, id, suffix)
}
func waitContext(ctx context.Context, ch <-chan struct{}) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ch:
		return nil
	}
}
