package main

import (
	"context"
	"sync"
	"time"
)

type cached struct {
	value card
	until time.Time
}
type flight struct {
	done  chan struct{}
	value card
	err   error
}
type cache struct {
	mu      sync.Mutex
	entries map[string]cached
	flights map[string]*flight
	max     int
	now     func() time.Time
}

func newCache(max int) *cache {
	return &cache{entries: map[string]cached{}, flights: map[string]*flight{}, max: max, now: time.Now}
}
func (c *cache) get(ctx context.Context, key string, force bool, fetch func() (card, error)) (card, error) {
	c.mu.Lock()
	now := c.now()
	for k, v := range c.entries {
		if !now.Before(v.until) {
			delete(c.entries, k)
		}
	}
	if v, ok := c.entries[key]; ok && !force {
		c.mu.Unlock()
		return v.value, nil
	}
	if f, ok := c.flights[key]; ok {
		c.mu.Unlock()
		if err := waitContext(ctx, f.done); err != nil {
			return card{}, err
		}
		return f.value, f.err
	}
	if len(c.flights) >= 64 {
		c.mu.Unlock()
		return card{}, failure(429, "busy", "Слишком много запросов. Попробуйте позже.")
	}
	f := &flight{done: make(chan struct{})}
	c.flights[key] = f
	c.mu.Unlock()
	go func() {
		v, e := fetch()
		c.mu.Lock()
		defer c.mu.Unlock()
		f.value = v
		f.err = e
		if e == nil {
			if len(c.entries) >= c.max {
				var oldest string
				var t time.Time
				for k, v := range c.entries {
					if oldest == "" || v.until.Before(t) {
						oldest = k
						t = v.until
					}
				}
				delete(c.entries, oldest)
			}
			c.entries[key] = cached{v, v.SourceFetchedAt.Add(serverTTL)}
		} else {
			delete(c.entries, key)
		}
		delete(c.flights, key)
		close(f.done)
	}()
	if err := waitContext(ctx, f.done); err != nil {
		return card{}, err
	}
	return f.value, f.err
}
func (c *cache) cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for k, v := range c.entries {
		if !now.Before(v.until) {
			delete(c.entries, k)
		}
	}
}
