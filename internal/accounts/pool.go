package accounts

import (
	"errors"
	"sort"
	"sync"
	"time"

	"zenbridge/internal/config"
)

var ErrNoAvailable = errors.New("no enabled upstream account available")

type affinityEntry struct {
	id      string
	touched time.Time
}

type Pool struct {
	mu       sync.Mutex
	accounts map[string]config.Account
	order    []string
	next     int
	affinity map[string]affinityEntry
}

func NewPool(accounts []config.Account) *Pool {
	p := &Pool{accounts: map[string]config.Account{}, affinity: map[string]affinityEntry{}}
	p.Reload(accounts)
	return p
}

func copyAccount(a config.Account) config.Account {
	a.Models = append([]string(nil), a.Models...)
	if a.Discovery != nil {
		d := *a.Discovery
		a.Discovery = &d
	}
	return a
}

func supports(a config.Account, model string) bool {
	for _, m := range a.Models {
		if m == model {
			return true
		}
	}
	return false
}

func (p *Pool) Reload(accounts []config.Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fresh := make(map[string]config.Account, len(accounts))
	order := make([]string, 0, len(accounts))
	for _, a := range accounts {
		if _, exists := fresh[a.ID]; exists {
			continue
		}
		fresh[a.ID] = copyAccount(a)
		order = append(order, a.ID)
	}
	p.accounts = fresh
	p.order = order
	if len(order) == 0 {
		p.next = 0
	} else {
		p.next %= len(order)
	}
}

func (p *Pool) Acquire(model, affinity string) (config.Account, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for key, e := range p.affinity {
		if now.Sub(e.touched) >= 15*time.Minute {
			delete(p.affinity, key)
		}
	}
	if affinity != "" {
		if e, ok := p.affinity[affinity]; ok {
			if a, exists := p.accounts[e.id]; exists && a.Enabled {
				e.touched = now
				p.affinity[affinity] = e
				return copyAccount(a), func() {}, nil
			}
		}
	}
	chosen := ""
	for offset := 0; offset < len(p.order); offset++ {
		index := (p.next + offset) % len(p.order)
		a := p.accounts[p.order[index]]
		if a.Enabled && supports(a, model) {
			chosen = a.ID
			p.next = (index + 1) % len(p.order)
			break
		}
	}
	if chosen == "" {
		for offset := 0; offset < len(p.order); offset++ {
			index := (p.next + offset) % len(p.order)
			a := p.accounts[p.order[index]]
			if a.Enabled {
				chosen = a.ID
				p.next = (index + 1) % len(p.order)
				break
			}
		}
	}
	if chosen == "" {
		return config.Account{}, nil, ErrNoAvailable
	}
	a := p.accounts[chosen]
	if affinity != "" {
		if _, exists := p.affinity[affinity]; !exists && len(p.affinity) >= 128 {
			var oldestKey string
			var oldest time.Time
			for k, e := range p.affinity {
				if oldest.IsZero() || e.touched.Before(oldest) {
					oldestKey, oldest = k, e.touched
				}
			}
			delete(p.affinity, oldestKey)
		}
		p.affinity[affinity] = affinityEntry{id: chosen, touched: now}
	}
	return copyAccount(a), func() {}, nil
}

func (p *Pool) Ready() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Enabled {
			return true
		}
	}
	return false
}

func (p *Pool) Models() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, a := range p.accounts {
		if a.Enabled {
			for _, m := range a.Models {
				seen[m] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func (p *Pool) AcquireAccount(id string) (config.Account, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.accounts[id]
	if !ok || !a.Enabled {
		return config.Account{}, nil, ErrNoAvailable
	}
	return copyAccount(a), func() {}, nil
}
