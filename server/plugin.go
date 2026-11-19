package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
)

type Plugin struct {
	plugin.MattermostPlugin
	mu       sync.RWMutex
	cfg      config
	cache    *cache
	upstream *upstream
	stop     chan struct{}
}
type mmStore struct{ p *Plugin }

func (s mmStore) Get(k string) ([]byte, error) {
	b, e := s.p.API.KVGet(k)
	if e != nil {
		return nil, errors.New("store unavailable")
	}
	return b, nil
}
func (s mmStore) CAS(k string, a, b []byte) (bool, error) {
	ok, e := s.p.API.KVCompareAndSet(k, a, b)
	if e != nil {
		return false, errors.New("store unavailable")
	}
	return ok, nil
}
func (p *Plugin) OnActivate() error {
	p.cache = newCache(2000)
	p.upstream = newUpstream()
	p.stop = make(chan struct{})
	if err := p.OnConfigurationChange(); err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-t.C:
				p.cache.cleanup()
			}
		}
	}()
	return nil
}
func (p *Plugin) OnDeactivate() error {
	if p.stop != nil {
		close(p.stop)
	}
	return nil
}
func (p *Plugin) OnConfigurationChange() error {
	var c config
	if err := p.API.LoadPluginConfiguration(&c); err != nil {
		return err
	}
	if c.BugzillaURL == "" {
		c.BugzillaURL = "https://bugzilla.altlinux.org"
	}
	c, err := normalizeConfig(c)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.cfg = c
	p.mu.Unlock()
	p.API.PublishWebSocketEvent("configuration_changed", nil, &model.WebsocketBroadcast{})
	return nil
}
func (p *Plugin) configuration() config { p.mu.RLock(); defer p.mu.RUnlock(); return p.cfg }
func (p *Plugin) secret(c config) ([]byte, error) {
	encoded := os.Getenv("MM_BUGZILLA_ENCRYPTION_KEY")
	path := os.Getenv("MM_BUGZILLA_ENCRYPTION_KEY_FILE")
	if path == "" {
		path = c.EncryptionKeyFile
	}
	if path != "" {
		b, e := os.ReadFile(path)
		if e != nil {
			return nil, errors.New("encryption secret unavailable")
		}
		encoded = string(b)
	}
	key, e := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if e != nil || len(key) != 32 {
		return nil, errors.New("encryption secret unavailable")
	}
	return key, nil
}
func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, err error) {
	var e *apiError
	if errors.As(err, &e) {
		jsonResponse(w, e.Status, map[string]string{"code": e.Code, "message": e.Message})
		return
	}
	jsonResponse(w, 500, map[string]string{"code": "internal", "message": "Не удалось выполнить запрос. Попробуйте позже."})
}
func (p *Plugin) ServeHTTP(ctx *plugin.Context, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	user := r.Header.Get("Mattermost-User-ID")
	if user == "" {
		writeError(w, failure(401, "unauthorized", "Войдите в Mattermost."))
		return
	}
	if ctx == nil || ctx.SessionId == "" {
		writeError(w, failure(401, "unauthorized", "Войдите в Mattermost."))
		return
	}
	session, sessionErr := p.API.GetSession(ctx.SessionId)
	if sessionErr != nil || session == nil || session.UserId != user || session.IsExpired() {
		writeError(w, failure(401, "unauthorized", "Войдите в Mattermost."))
		return
	}
	if r.Method != http.MethodGet && r.Header.Get("X-Requested-With") != "XMLHttpRequest" {
		writeError(w, failure(403, "csrf", "Недопустимый источник запроса."))
		return
	}
	c := p.configuration()
	store := mmStore{p}
	conn, err := loadConnection(store, c, user)
	if err != nil {
		writeError(w, err)
		return
	}
	switch r.URL.Path {
	case "/api/v1/config":
		if r.Method != http.MethodGet {
			w.WriteHeader(405)
			return
		}
		_, secErr := p.secret(c)
		jsonResponse(w, 200, map[string]any{"url": c.BugzillaURL, "instance": configIdentity(c), "revision": conn.Revision, "connected": conn.Ciphertext != "", "can_connect": secErr == nil})
		return
	case "/api/v1/connection":
		if r.Method == http.MethodDelete {
			if err = storeConnection(store, c, user, connection{}); err != nil {
				writeError(w, err)
				return
			}
			p.API.PublishWebSocketEvent("connection_changed", nil, &model.WebsocketBroadcast{UserId: user})
			jsonResponse(w, 200, map[string]bool{"ok": true})
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			writeError(w, failure(415, "content_type", "Ожидается JSON."))
			return
		}
		var payload struct {
			APIKey string `json:"api_key"`
		}
		d := json.NewDecoder(io.LimitReader(r.Body, 4097))
		d.DisallowUnknownFields()
		if d.Decode(&payload) != nil || len(payload.APIKey) < 8 || len(payload.APIKey) > 512 || strings.ContainsAny(payload.APIKey, "\r\n") {
			writeError(w, failure(400, "invalid_input", "Введите личный API-ключ Bugzilla."))
			return
		}
		secret, e := p.secret(c)
		if e != nil {
			writeError(w, failure(503, "encryption_unavailable", "Администратор должен настроить серверный секрет шифрования."))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if e = p.upstream.validate(ctx, c, payload.APIKey); e != nil {
			writeError(w, e)
			return
		}
		encrypted, e := encrypt(secret, payload.APIKey, connectionKey(c, user))
		if e != nil {
			writeError(w, e)
			return
		}
		if configIdentity(c) != configIdentity(p.configuration()) {
			writeError(w, errChanged)
			return
		}
		if e = storeConnection(store, c, user, connection{Ciphertext: encrypted}); e != nil {
			writeError(w, e)
			return
		}
		p.API.PublishWebSocketEvent("connection_changed", nil, &model.WebsocketBroadcast{UserId: user})
		jsonResponse(w, 200, map[string]bool{"ok": true})
		return
	case "/api/v1/card":
		if r.Method != http.MethodGet {
			w.WriteHeader(405)
			return
		}
		id, e := strconv.Atoi(r.URL.Query().Get("id"))
		if e != nil || id <= 0 {
			writeError(w, failure(400, "invalid_id", "Некорректный номер бага."))
			return
		}
		var count *int
		if raw, ok := r.URL.Query()["comment"]; ok {
			if len(raw) != 1 {
				writeError(w, failure(400, "invalid_comment", "Некорректный номер комментария."))
				return
			}
			v, e := strconv.Atoi(raw[0])
			if e != nil || v < 0 {
				writeError(w, failure(400, "invalid_comment", "Некорректный номер комментария."))
				return
			}
			count = &v
		}
		key := ""
		if conn.Ciphertext != "" {
			secret, e := p.secret(c)
			if e != nil {
				writeError(w, failure(503, "encryption_unavailable", "Секрет шифрования недоступен."))
				return
			}
			key, e = decrypt(secret, conn.Ciphertext, connectionKey(c, user))
			if e != nil {
				writeError(w, failure(503, "encryption_unavailable", "Не удалось прочитать личный ключ. Подключите аккаунт заново."))
				return
			}
		}
		v, e := p.cache.get(r.Context(), cacheKey(user, c, conn.Revision, id, count), r.URL.Query().Get("refresh") == "true", func() (card, error) {
			out, err := p.upstream.fetch(c, key, id, count)
			if err != nil {
				return card{}, err
			}
			current, err := loadConnection(store, c, user)
			if err != nil {
				return card{}, err
			}
			if current.Revision != conn.Revision || configIdentity(c) != configIdentity(p.configuration()) {
				return card{}, errChanged
			}
			out.Revision = conn.Revision
			return out, nil
		})
		if e != nil {
			writeError(w, e)
			return
		}
		current, e := loadConnection(store, c, user)
		if e != nil {
			writeError(w, e)
			return
		}
		if current.Revision != conn.Revision || configIdentity(c) != configIdentity(p.configuration()) {
			writeError(w, errChanged)
			return
		}
		v.MaxAgeMS = time.Until(v.SourceFetchedAt.Add(clientTTL)).Milliseconds()
		if v.MaxAgeMS < 0 {
			v.MaxAgeMS = 0
		}
		jsonResponse(w, 200, v)
		return
	default:
		w.WriteHeader(404)
	}
}
