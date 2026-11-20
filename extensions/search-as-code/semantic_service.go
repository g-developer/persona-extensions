package main

import (
	"context"
	"sync"
	"time"
)

// vecService owns the hub's central vector index (vectors.go) for the current settings and keeps it fresh: its
// own computer's sessions and the synced index of every picked computer.
type vecService struct {
	mu                sync.Mutex
	dir               string
	mode, url, model  string
	idx               *vectorIndex
	lastRefresh       time.Time
	refreshEveryLocal time.Duration
	// readOnly: another process (the hub's extension) owns the index file; never refresh or write it.
	readOnly bool
}

const vecRefreshEvery = 5 * time.Minute

func newVecService() *vecService { return &vecService{refreshEveryLocal: 2 * time.Minute} }

// configure applies the settings; a change of embedder opens that embedder's index (each has its own file).
func (v *vecService) configure(dir, mode, url, model string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.idx != nil && v.dir == dir && v.mode == mode && v.url == url && v.model == model {
		return
	}
	v.dir, v.mode, v.url, v.model = dir, mode, url, model
	v.idx, v.lastRefresh = nil, time.Time{}
	if emb := newEmbedder(mode, url, model); emb != nil {
		v.idx = openVectorIndex(dir, emb)
	}
}

func (v *vecService) index() *vectorIndex {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.idx
}

// refresh indexes the hub's sessions and the synced sessions of the picked computers, then saves the index.
func (v *vecService) refresh(ctx context.Context, st *State, f *fleet) error {
	idx := v.index()
	if idx == nil {
		return nil
	}
	local, _ := loadSessions(st)
	sets := []sessionSet{{Machine: "", Sessions: local}}
	if f.hasRemote() {
		for _, t := range f.targets(ctx) {
			if t.Local {
				continue
			}
			if c := f.loadCache(t.ID); c != nil {
				sets = append(sets, sessionSet{Machine: t.ID, Sessions: c.Sessions})
			}
		}
	}
	err := idx.refresh(ctx, sets)
	if serr := idx.save(); err == nil {
		err = serr
	}
	v.mu.Lock()
	v.lastRefresh = time.Now()
	v.mu.Unlock()
	return err
}

// freshEnough refreshes the index before a search when it is built in (hashing is cheap) and older than a
// couple of minutes; an index with an embedding endpoint is refreshed in the background only.
func (v *vecService) freshEnough(ctx context.Context, st *State, f *fleet) {
	idx := v.index()
	if idx == nil {
		return
	}
	if _, ok := idx.emb.(hashEmbedder); !ok {
		return
	}
	v.mu.Lock()
	stale := !v.readOnly && time.Since(v.lastRefresh) > v.refreshEveryLocal
	v.mu.Unlock()
	if stale {
		_ = v.refresh(ctx, st, f)
	}
}

// loop refreshes shortly after start and then every vecRefreshEvery, until stop closes.
func (v *vecService) loop(stop chan struct{}, st func() *State, f *fleet) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-stop
		cancel()
	}()
	first := time.NewTimer(20 * time.Second)
	defer first.Stop()
	tick := time.NewTicker(vecRefreshEvery)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-first.C:
		case <-tick.C:
		}
		if s := st(); s != nil {
			_ = v.refresh(ctx, s, f)
		}
	}
}

// hints runs the vector stage of a search over the given computers (keys of targets; "" the hub's own).
func (v *vecService) hints(ctx context.Context, st *State, f *fleet, prog SearchProgram, now time.Time, targets []target) semanticResult {
	idx := v.index()
	if idx == nil {
		return semanticResult{}
	}
	v.freshEnough(ctx, st, f)
	names := map[string]string{"": localMachineName}
	allowed := map[string]bool{}
	for _, t := range targets {
		if t.Local {
			allowed[""] = true
			names[""] = t.Name
		} else {
			allowed[t.ID] = true
			names[t.ID] = t.Name
		}
	}
	if len(targets) == 0 {
		allowed[""] = true
	}
	return idx.semanticHints(ctx, prog, now, func(id string) string { return names[id] }, func(id string) bool { return allowed[id] })
}

func (r semanticResult) apply(res *ExecutionResult) *ExecutionResult {
	if r.enabled && res != nil {
		prependTrace(res, r.trace)
		if r.hints != nil {
			res.Semantic = true
		}
	}
	return res
}
