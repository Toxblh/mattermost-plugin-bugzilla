package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type upstream struct {
	client *http.Client
	slots  chan struct{}
}

func newUpstream() *upstream {
	return &upstream{client: &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}, slots: make(chan struct{}, 8)}
}
func (u *upstream) get(ctx context.Context, c config, key, path string, params url.Values, out any) error {
	select {
	case u.slots <- struct{}{}:
		defer func() { <-u.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if params == nil {
		params = url.Values{}
	}
	if key != "" && c.APIKeyTransport == "query" {
		params.Set("api_key", key)
	}
	target := c.BugzillaURL + "/rest/" + path + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return failure(502, "upstream", "Не удалось выполнить запрос Bugzilla.")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mattermost-Bugzilla-Links/0.1")
	if key != "" && c.APIKeyTransport == "header" {
		req.Header.Set("X-BUGZILLA-API-KEY", key)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return failure(502, "upstream", "Bugzilla не отвечает. Попробуйте позже.")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
	if err != nil || len(body) > 2*1024*1024 {
		return failure(502, "upstream", "Ответ Bugzilla слишком большой или недоступен.")
	}
	var problem struct {
		Error bool `json:"error"`
		Code  int  `json:"code"`
	}
	_ = json.Unmarshal(body, &problem)
	if problem.Error {
		switch problem.Code {
		case 300, 301, 302, 303, 304, 305, 306, 307, 308, 32000, 410, 505:
			return failure(401, "invalid_key", "Ключ Bugzilla недействителен. Подключите аккаунт заново.")
		case 100, 101, 102, 109, 110, 111, 112:
			return failure(404, "unavailable", "Баг или комментарий недоступен.")
		default:
			return failure(502, "upstream", "Bugzilla отклонила запрос.")
		}
	}
	if resp.StatusCode == 401 {
		return failure(401, "invalid_key", "Ключ Bugzilla недействителен.")
	}
	if resp.StatusCode == 403 || resp.StatusCode == 404 {
		return failure(404, "unavailable", "Баг или комментарий недоступен.")
	}
	if resp.StatusCode != 200 {
		return failure(502, "upstream", "Ошибка ответа Bugzilla.")
	}
	if json.Unmarshal(body, out) != nil {
		return failure(502, "upstream", "Bugzilla вернула не JSON. Проверьте REST API и защиту сервера.")
	}
	return nil
}

// User.match is authenticated-only in Bugzilla 5.2. A random impossible match
// validates the key without requiring a login or enumerating users. Identity is
// the owner of the validated key; we deliberately do not claim a user ID.
func (u *upstream) validate(ctx context.Context, c config, key string) error {
	var response struct {
		Users *[]json.RawMessage `json:"users"`
	}
	err := u.get(ctx, c, key, "user", url.Values{"match": {"mm-plugin-key-check-" + randomID()}, "limit": {"1"}, "include_fields": {"id"}}, &response)
	if err != nil {
		return err
	}
	if response.Users == nil {
		return failure(502, "upstream", "Bugzilla не подтвердила авторизацию ключа.")
	}
	return nil
}
func (u *upstream) fetch(c config, key string, id int, count *int) (card, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()
	var response struct {
		Bugs []bug `json:"bugs"`
	}
	if err := u.get(ctx, c, key, "bug/"+strconv.Itoa(id), url.Values{"include_fields": {"id,summary,status,resolution,priority,assigned_to,product,component,last_change_time"}}, &response); err != nil {
		return card{}, err
	}
	if len(response.Bugs) != 1 || response.Bugs[0].ID != id {
		return card{}, failure(404, "unavailable", "Баг недоступен.")
	}
	out := card{Bug: response.Bugs[0], URL: fmt.Sprintf("%s/show_bug.cgi?id=%d", c.BugzillaURL, id), Mode: "public"}
	if key != "" {
		out.Mode = "personal"
	}
	// Bugzilla description is comment #0. Request only allowed comment fields; never return the complete list.
	var comments struct {
		Bugs map[string]struct {
			Comments []comment `json:"comments"`
		} `json:"bugs"`
	}
	err := u.get(ctx, c, key, fmt.Sprintf("bug/%d/comment", id), url.Values{"include_fields": {"id,count,text,creator,creation_time,is_private"}}, &comments)
	if err != nil {
		if count != nil {
			return card{}, err
		}
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == 401 {
			return card{}, err
		}
	} else {
		for _, v := range comments.Bugs[strconv.Itoa(id)].Comments {
			if v.Private && key == "" {
				continue
			}
			if v.Count == 0 {
				out.Description = truncate(v.Text, 800)
			}
			if count != nil && v.Count == *count {
				copy := v
				copy.Text = truncate(copy.Text, 4000)
				out.Comment = &copy
			}
		}
	}
	if count != nil {
		if out.Comment == nil {
			return card{}, failure(404, "unavailable", "Комментарий недоступен.")
		}
		out.URL += fmt.Sprintf("#c%d", *count)
	}
	out.Bug.Summary = truncate(out.Bug.Summary, 500)
	out.SourceFetchedAt = time.Now().UTC()
	return out, nil
}
