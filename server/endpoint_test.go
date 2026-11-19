package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
)

type memoryStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (m *memoryStore) Get(k string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.data[k]...), nil
}
func (m *memoryStore) CAS(k string, a, b []byte) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !bytes.Equal(m.data[k], a) {
		return false, nil
	}
	m.data[k] = append([]byte(nil), b...)
	return true, nil
}

type testAPI struct {
	plugin.API
	store   *memoryStore
	user    string
	expired bool
}

func (a *testAPI) KVGet(k string) ([]byte, *model.AppError) { b, _ := a.store.Get(k); return b, nil }
func (a *testAPI) KVCompareAndSet(k string, old, new []byte) (bool, *model.AppError) {
	ok, _ := a.store.CAS(k, old, new)
	return ok, nil
}
func (a *testAPI) GetSession(id string) (*model.Session, *model.AppError) {
	expires := time.Now().Add(time.Hour).UnixMilli()
	if a.expired {
		expires = 1
	}
	return &model.Session{Id: id, UserId: a.user, ExpiresAt: expires}, nil
}
func (a *testAPI) PublishWebSocketEvent(_ string, _ map[string]interface{}, _ *model.WebsocketBroadcast) {
}
func makeTestPlugin(t *testing.T, up *httptest.Server, store *memoryStore, user string) *Plugin {
	t.Helper()
	p := &Plugin{cfg: config{BugzillaURL: up.URL, APIKeyTransport: "query"}, cache: newCache(20), upstream: newUpstream()}
	p.upstream.client = up.Client()
	p.API = &testAPI{store: store, user: user}
	return p
}
func request(t *testing.T, p *Plugin, user, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Mattermost-User-ID", user)
	r.Header.Set("X-Requested-With", "XMLHttpRequest")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.ServeHTTP(&plugin.Context{SessionId: "session"}, w, r)
	return w
}
func TestHARevisionInvalidatesAnotherNodeCache(t *testing.T) {
	t.Setenv("MM_BUGZILLA_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	var mu sync.Mutex
	calls := 0
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/user":
			w.Write([]byte(`{"users":[]}`))
		case "/rest/bug/123":
			mu.Lock()
			calls++
			mu.Unlock()
			title := "public"
			if r.URL.Query().Get("api_key") == "personal-key" {
				title = "private-title"
			}
			json.NewEncoder(w).Encode(map[string]any{"bugs": []map[string]any{{"id": 123, "summary": title}}})
		case "/rest/bug/123/comment":
			w.Write([]byte(`{"bugs":{"123":{"comments":[]}}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer s.Close()
	store := &memoryStore{data: map[string][]byte{}}
	a, b := makeTestPlugin(t, s, store, "alice"), makeTestPlugin(t, s, store, "alice")
	if w := request(t, a, "alice", "POST", "/api/v1/connection", `{"api_key":"personal-key"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for i := 0; i < 2; i++ {
		w := request(t, b, "alice", "GET", "/api/v1/card?id=123", "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "private-title") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 1 {
		t.Fatal("cache missed", n)
	}
	if w := request(t, a, "alice", "DELETE", "/api/v1/connection", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w := request(t, b, "alice", "GET", "/api/v1/card?id=123", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "private-title") || !strings.Contains(w.Body.String(), `"mode":"public"`) {
		t.Fatal("other node served old private cache", w.Code, w.Body.String())
	}
	if len(b.cache.entries) < 2 {
		t.Fatal("disconnect unexpectedly physically cleared other-node cache")
	}
	b.API.(*testAPI).expired = true
	w = request(t, b, "alice", "GET", "/api/v1/card?id=123", "")
	if w.Code != 401 {
		t.Fatal("expired session read cache", w.Code)
	}
}
func TestMutationRejectsMissingCSRFHeader(t *testing.T) {
	s := httptest.NewTLSServer(http.NotFoundHandler())
	defer s.Close()
	p := makeTestPlugin(t, s, &memoryStore{data: map[string][]byte{}}, "alice")
	r := httptest.NewRequest("DELETE", "/api/v1/connection", nil)
	r.Header.Set("Mattermost-User-ID", "alice")
	w := httptest.NewRecorder()
	p.ServeHTTP(&plugin.Context{SessionId: "session"}, w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}
func TestConnectionRevisionChangesAndCiphertextBoundToInstance(t *testing.T) {
	store := &memoryStore{data: map[string][]byte{}}
	c := config{BugzillaURL: "https://bugs.example", APIKeyTransport: "query"}
	before, _ := loadConnection(store, c, "alice")
	if before.Revision != "public" {
		t.Fatal(before)
	}
	if e := storeConnection(store, c, "alice", connection{Ciphertext: "encrypted"}); e != nil {
		t.Fatal(e)
	}
	a, _ := loadConnection(store, c, "alice")
	_ = storeConnection(store, c, "alice", connection{})
	b, _ := loadConnection(store, c, "alice")
	if a.Revision == b.Revision || b.Revision == "public" || b.Ciphertext != "" {
		t.Fatal("revision reset or unchanged")
	}
	other, _ := loadConnection(store, c, "bob")
	if other.Ciphertext != "" {
		t.Fatal("cross-user connection")
	}
	c.BugzillaURL = "https://other.example"
	other, _ = loadConnection(store, c, "alice")
	if other.Ciphertext != "" {
		t.Fatal("cross-instance connection")
	}
}
func TestInflightLimitAndFailedResponsesNotCached(t *testing.T) {
	c := newCache(4)
	for i := 0; i < 64; i++ {
		c.flights[string(rune(i))] = &flight{done: make(chan struct{})}
	}
	_, err := c.get(t.Context(), "extra", false, func() (card, error) { t.Fatal("fetch started above cap"); return card{}, nil })
	var ae *apiError
	if !errors.As(err, &ae) || ae.Status != 429 {
		t.Fatal(err)
	}
	c = newCache(4)
	calls := 0
	for i := 0; i < 2; i++ {
		_, _ = c.get(t.Context(), "error", false, func() (card, error) { calls++; return card{}, errors.New("fail") })
	}
	if calls != 2 || len(c.entries) != 0 {
		t.Fatal("error was cached")
	}
}
