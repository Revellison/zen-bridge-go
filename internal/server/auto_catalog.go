package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"time"

	"zenbridge/internal/catalog"
	"zenbridge/internal/config"
)

type autoCatalogState struct {
	generation   [32]byte
	nextRefresh  time.Time
	nextCheck    time.Time
	blockedUntil time.Time
}

func (s *Server) RunAutoCatalog(ctx context.Context) {
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	states := map[string]*autoCatalogState{}
	s.autoCatalogTick(ctx, time.Now(), states)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.autoCatalogTick(ctx, now, states)
		}
	}
}

func (s *Server) autoCatalogTick(ctx context.Context, now time.Time, states map[string]*autoCatalogState) {
	if ctx.Err() != nil {
		return
	}
	accounts, err := config.LoadConfiguredAccounts(s.cfg)
	if err != nil {
		return
	}
	active := map[string]bool{}
	for _, a := range accounts {
		if a.Enabled && a.AuthMode == "key" && a.Discovery != nil && a.Discovery.Enabled {
			active[a.ID] = true
		}
	}
	for id := range states {
		if !active[id] {
			delete(states, id)
		}
	}
	for _, a := range accounts {
		if !active[a.ID] {
			continue
		}
		d := a.Discovery
		revision, _ := json.Marshal([]any{catalog.Tag(a), d})
		generation := sha256.Sum256(revision)
		st := states[a.ID]
		if st == nil {
			st = &autoCatalogState{generation: generation}
			states[a.ID] = st
		} else if st.generation != generation {
			st.generation = generation
			st.nextRefresh = now.Add(time.Duration(d.RefreshIntervalSeconds) * time.Second)
			st.nextCheck = now.Add(time.Duration(d.CheckIntervalSeconds) * time.Second)
		}
		if now.Before(st.blockedUntil) {
			continue
		}
		refresh := !now.Before(st.nextRefresh)
		check := d.CheckEnabled && !now.Before(st.nextCheck)
		if !refresh && !check {
			continue
		}
		reserved, release, e := s.pool.AcquireAccount(a.ID)
		if e != nil {
			continue
		}
		if catalog.Tag(reserved) != catalog.Tag(a) {
			release()
			continue
		}
		report, e := catalog.Run(ctx, s.cfg, a.ID, check, catalog.Options{MaxChecks: d.MaxChecksPerRun, OnlyStale: true, Refresh: refresh, ExpectedTag: catalog.Tag(reserved)})
		end := time.Now()
		if end.Before(now) {
			end = now
		}
		backoff := time.Duration(0)
		if refresh {
			st.nextRefresh = end.Add(time.Duration(d.RefreshIntervalSeconds) * time.Second)
		}
		if check {
			st.nextCheck = end.Add(time.Duration(d.CheckIntervalSeconds) * time.Second)
		}
		failed := e != nil
		for _, m := range report.Models {
			if m.CheckedAt.Before(now) {
				continue
			}
			if m.Status == "transient" || m.Status == "error" {
				failed = true
			}
			if m.HTTPStatus == 401 || m.HTTPStatus == 403 {
				failed = true
			}
		}
		if failed {
			backoff = time.Duration(d.FailureBackoffSeconds) * time.Second
			if retry := time.Duration(report.RetryAfterSeconds) * time.Second; retry > backoff {
				backoff = retry
			}
			st.blockedUntil = end.Add(backoff)
			if st.nextCheck.Before(st.blockedUntil) {
				st.nextCheck = st.blockedUntil
			}
			if st.nextRefresh.Before(st.blockedUntil) {
				st.nextRefresh = st.blockedUntil
			}
		}
		release()
		s.logger.Info("automatic catalog pass", "account", a.ID, "failed", e != nil, "counts", report.Counts, "truncated", report.Truncated)
		return
	}
}
