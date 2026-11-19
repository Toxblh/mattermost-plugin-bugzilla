package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReferenceParsing(t *testing.T) {
	base := "https://bugzilla.altlinux.org"
	for _, raw := range []string{base + "/58948", base + "/show_bug.cgi?id=58948", base + "/show_bug.cgi?id=58948#c15"} {
		id, count, err := parseReference(raw, base)
		if err != nil || id != 58948 {
			t.Fatalf("%s: %v", raw, err)
		}
		if strings.HasSuffix(raw, "c15") && (count == nil || *count != 15) {
			t.Fatal("comment number lost")
		}
	}
	for _, raw := range []string{"https://bugzilla.altlinux.org.evil/123", "https://evil@bugzilla.altlinux.org/123", base + "/show_bug.cgi?id=1&id=2", base + "/0", base + "/01", base + "/123/x", base + "/1#c-1"} {
		if _, _, e := parseReference(raw, base); e == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
func TestEncryptionBoundToOwner(t *testing.T) {
	key := make([]byte, 32)
	value, err := encrypt(key, "secret-api-key", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(value, "secret-api-key") {
		t.Fatal("plaintext")
	}
	plain, err := decrypt(key, value, "alice")
	if err != nil || plain != "secret-api-key" {
		t.Fatal(err)
	}
	if _, err = decrypt(key, value, "bob"); err == nil {
		t.Fatal("cross-user decryption")
	}
	b := []byte(value)
	b[len(b)/2] ^= 1
	if _, err = decrypt(key, string(b), "alice"); err == nil {
		t.Fatal("tamper accepted")
	}
}
func TestCacheIsolationExpiryAndSourceTime(t *testing.T) {
	c := newCache(5)
	now := time.Now()
	c.now = func() time.Time { return now }
	var calls int
	fetch := func() (card, error) { calls++; return card{SourceFetchedAt: now, Bug: bug{ID: calls}}, nil }
	a, _ := c.get(context.Background(), "alice", false, fetch)
	now = now.Add(80 * time.Second)
	b, _ := c.get(context.Background(), "alice", false, fetch)
	if calls != 1 || !a.SourceFetchedAt.Equal(b.SourceFetchedAt) {
		t.Fatal("hit extended freshness")
	}
	_, _ = c.get(context.Background(), "bob", false, fetch)
	if calls != 2 {
		t.Fatal("users shared result")
	}
	now = now.Add(11 * time.Second)
	_, _ = c.get(context.Background(), "alice", false, fetch)
	if calls != 3 {
		t.Fatal("expired hit")
	}
	_, _ = c.get(context.Background(), "alice", true, fetch)
	if calls != 4 {
		t.Fatal("force used cache")
	}
}
func TestSingleflight(t *testing.T) {
	c := newCache(4)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	fetch := func() (card, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return card{SourceFetchedAt: time.Now()}, nil
	}
	var wg sync.WaitGroup
	wg.Add(10)
	for i := 0; i < 10; i++ {
		go func() {
			defer wg.Done()
			_, e := c.get(context.Background(), "same", false, fetch)
			if e != nil {
				t.Error(e)
			}
		}()
	}
	<-started
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("calls %d", calls.Load())
	}
}
func TestKeyOnlyValidationRejectsJSONErrorsAndMalformed(t *testing.T) {
	for _, body := range []string{`{"error":true,"code":306}`, `{"error":true,"code":505}`, `<html>challenge</html>`, `true`, `{}`} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
			defer s.Close()
			u := newUpstream()
			u.client = s.Client()
			if e := u.validate(context.Background(), config{BugzillaURL: s.URL, APIKeyTransport: "query"}, "private-key"); e == nil {
				t.Fatal("accepted invalid response")
			}
		})
	}
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/user" || r.URL.Query().Get("api_key") != "private-key" || r.URL.Query().Get("limit") != "1" || r.URL.Query().Get("include_fields") != "id" || !strings.HasPrefix(r.URL.Query().Get("match"), "mm-plugin-key-check-") {
			t.Errorf("incorrect validation request %s", r.URL.Path)
		}
		w.Write([]byte(`{"users":[]}`))
	}))
	defer s.Close()
	u := newUpstream()
	u.client = s.Client()
	if e := u.validate(context.Background(), config{BugzillaURL: s.URL, APIKeyTransport: "query"}, "private-key"); e != nil {
		t.Fatal(e)
	}
}
func TestCommentLocalCountAndNoPrivateLeak(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/bug/54753":
			w.Write([]byte(`{"bugs":[{"id":54753,"summary":"public bug"}]}`))
		case "/rest/bug/54753/comment":
			w.Write([]byte(`{"bugs":{"54753":{"comments":[{"id":15,"count":1,"text":"wrong global id"},{"id":100015,"count":15,"text":"secret","is_private":true},{"id":200,"count":0,"text":"description"}]}}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer s.Close()
	u := newUpstream()
	u.client = s.Client()
	c := config{BugzillaURL: s.URL, APIKeyTransport: "query"}
	n := 15
	if _, e := u.fetch(c, "", 54753, &n); e == nil {
		t.Fatal("private comment leaked anonymously")
	}
	out, e := u.fetch(c, "personal-key", 54753, &n)
	if e != nil || out.Comment == nil || out.Comment.ID != 100015 {
		t.Fatalf("count vs global ID: %+v %v", out, e)
	}
	if out.Description != "description" {
		t.Fatal("missing description")
	}
}
func TestRedirectDoesNotForwardKey(t *testing.T) {
	var leaked atomic.Bool
	dest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer dest.Close()
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, dest.URL, 302) }))
	defer s.Close()
	u := newUpstream()
	u.client = s.Client()
	u.client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	var out any
	if e := u.get(context.Background(), config{BugzillaURL: s.URL, APIKeyTransport: "query"}, "secret", "user", url.Values{}, &out); e == nil {
		t.Fatal("redirect accepted")
	}
	if leaked.Load() {
		t.Fatal("followed redirect")
	}
}
func TestUnauthenticatedRequestNeverReadsCache(t *testing.T) {
	p := &Plugin{}
	r := httptest.NewRequest("GET", "/api/v1/card?id=1", nil)
	w := httptest.NewRecorder()
	p.ServeHTTP(nil, w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing private cache policy")
	}
}
func TestPublicTransportDoesNotSendKey(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("api_key") || r.Header.Get("X-BUGZILLA-API-KEY") != "" {
			t.Fatal("key sent")
		}
		json.NewEncoder(w).Encode(map[string]any{"version": "5.2"})
	}))
	defer s.Close()
	u := newUpstream()
	u.client = s.Client()
	var result map[string]any
	if e := u.get(context.Background(), config{BugzillaURL: s.URL, APIKeyTransport: "query"}, "", "version", nil, &result); e != nil {
		t.Fatal(e)
	}
}
