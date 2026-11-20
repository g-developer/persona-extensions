package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// The central vector index (hub only).
//
// Every computer keeps searching its own transcripts with keywords (federated: the program runs where the
// sessions are, lexical.go). Vectors are kept in one place, the hub's data directory: the hub embeds the
// conversational exchanges of its own sessions and of the synced index of every picked computer (remote.go), so
// nodes need no model and an offline computer is searched semantically all the same. A search embeds the query
// once at the hub, finds the closest exchanges across all computers, and hands each computer the sessions found
// on it with their similarity (semHint). Each computer then fuses that similarity with its keyword relevance in
// scan_sessions and runs the rest of the pipeline over its full transcripts as before.
//
// The similarities come from one index and one query, so they are comparable across computers, and fusion is a
// weighted sum of two scores in [0, 1] rather than a rank fusion: merged results of different computers stay
// ordered by one scale (rank fusion such as RRF does not compose across partitions).

// Search modes of scan_sessions.
const (
	modeHybrid   = "hybrid"   // keyword relevance fused with the hub's vector similarity (default)
	modeLexical  = "lexical"  // keywords only
	modeSemantic = "semantic" // vector similarity only
)

var searchModes = []string{modeHybrid, modeLexical, modeSemantic}

func searchMode(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if slices.Contains(searchModes, s) {
		return s
	}
	return modeHybrid
}

// Fusion weights of a hybrid search.
const (
	lexWeight = 0.6
	semWeight = 0.4
)

const (
	vecChunkRunes    = 1200 // an exchange (a user turn and the replies to it), clipped
	vecTopSessions   = 80   // sessions a query hands to the computers
	vecMinConfidence = 0.12
	vecEmbedBatch    = 32
	vecMaxPerRefresh = 4000 // chunks embedded per refresh with a remote endpoint (the rest next time)
	vecFormat        = 2
)

// semHint is a session the hub's vector index found close to a query, on the computer it is sent to.
type semHint struct {
	SessionID string  `json:"session_id"`
	Sim       float64 `json:"sim"` // confidence in [0, 1], comparable across computers of one search
	Line      int     `json:"line,omitempty"`
}

// vecChunk is one embedded exchange.
type vecChunk struct {
	Hash uint64
	Line int
	Vec  []int8
	Inv  float32 // 1 / quantization scale
}

// vecSession is one session of the index with the metadata a search filters on.
type vecSession struct {
	Machine, SessionID           string
	Project, CWD, Harness, Model string
	UpdatedAt                    time.Time
	Chunks                       []vecChunk
}

type vecFile struct {
	Format   int
	Embedder string
	Sessions []*vecSession
}

// vectorIndex is the hub's index for one embedder, in <data>/vectors/<hash of the embedder id>.gob.
type vectorIndex struct {
	emb  embedder
	path string

	mu       sync.RWMutex
	sessions map[string]*vecSession // by machine + "\x00" + session id
	dirty    bool
	builtAt  time.Time
	lastErr  string

	refreshMu sync.Mutex
}

func vecKey(machine, sid string) string { return machine + "\x00" + sid }

func vectorPath(dir string, emb embedder) string {
	sum := sha256.Sum256([]byte(emb.ID()))
	return filepath.Join(dir, "vectors", hex.EncodeToString(sum[:8])+".gob")
}

// openVectorIndex loads the index of emb from dir (empty when there is none yet).
func openVectorIndex(dir string, emb embedder) *vectorIndex {
	v := &vectorIndex{emb: emb, sessions: map[string]*vecSession{}}
	if dir != "" {
		v.path = vectorPath(dir, emb)
		if f, err := os.Open(v.path); err == nil {
			var vf vecFile
			if gob.NewDecoder(f).Decode(&vf) == nil && vf.Format == vecFormat && vf.Embedder == emb.ID() {
				for _, s := range vf.Sessions {
					v.sessions[vecKey(s.Machine, s.SessionID)] = s
				}
			}
			f.Close()
		}
	}
	return v
}

func (v *vectorIndex) save() error {
	v.mu.Lock()
	if !v.dirty || v.path == "" {
		v.mu.Unlock()
		return nil
	}
	vf := vecFile{Format: vecFormat, Embedder: v.emb.ID()}
	for _, s := range v.sessions {
		vf.Sessions = append(vf.Sessions, s)
	}
	v.dirty = false
	v.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(v.path), 0o700); err != nil {
		return err
	}
	tmp := v.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := gob.NewEncoder(f).Encode(&vf); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, v.path)
}

// vecStats describes the index for the page and the step trace.
type vecStats struct {
	Embedder string    `json:"embedder"`
	Sessions int       `json:"sessions"`
	Chunks   int       `json:"chunks"`
	Machines int       `json:"machines"`
	BuiltAt  time.Time `json:"built_at,omitzero"`
	Error    string    `json:"error,omitempty"`
}

func (v *vectorIndex) stats() vecStats {
	v.mu.RLock()
	defer v.mu.RUnlock()
	st := vecStats{Embedder: v.emb.ID(), Sessions: len(v.sessions), BuiltAt: v.builtAt, Error: v.lastErr}
	machines := map[string]bool{}
	for _, s := range v.sessions {
		st.Chunks += len(s.Chunks)
		machines[s.Machine] = true
	}
	st.Machines = len(machines)
	return st
}

// chunkText is an exchange to embed.
type chunkText struct {
	Line int
	Text string
	Hash uint64
}

// sessionChunks splits a session's conversation into exchanges: a user turn and the assistant turns that answer
// it, clipped; tool calls and outputs are left out (keyword search covers them).
func sessionChunks(s *SessionRecord) []chunkText {
	var out []chunkText
	var b strings.Builder
	line := 0
	flush := func() {
		// A long exchange gets its head, middle and tail as separate vectors: embedding only its beginning
		// misses what the answer concluded.
		for _, t := range windows(strings.TrimSpace(b.String()), vecChunkRunes, 3) {
			sum := sha256.Sum256([]byte(t))
			out = append(out, chunkText{Line: line, Text: t, Hash: binary.BigEndian.Uint64(sum[:8])})
		}
		b.Reset()
		line = 0
	}
	if t := strings.TrimSpace(s.Title); t != "" {
		b.WriteString(t)
		b.WriteString("\n")
	}
	for _, tr := range s.Turns {
		if tr.IsTool || (tr.Role != "user" && tr.Role != "assistant") {
			continue
		}
		if tr.Role == "user" && line != 0 {
			flush()
		}
		if line == 0 {
			line = tr.Line
		}
		if b.Len() < vecChunkRunes*12 {
			b.WriteString(oneLine(tr.Text))
			b.WriteString("\n")
		}
	}
	flush()
	return out
}

// windows splits text into at most n windows of size runes: the head, evenly spaced middles and the tail.
func windows(text string, size, n int) []string {
	if text == "" {
		return nil
	}
	r := []rune(text)
	if len(r) <= size {
		return []string{text}
	}
	count := min(n, (len(r)+size-1)/size)
	out := make([]string, 0, count)
	for i := range count {
		start := 0
		if count > 1 {
			start = i * (len(r) - size) / (count - 1)
		}
		out = append(out, string(r[start:start+size]))
	}
	return out
}

// sessionSet is the sessions of one computer to index.
type sessionSet struct {
	Machine  string
	Sessions []*SessionRecord
}

// refresh brings the index up to the given sessions: new exchanges are embedded (an exchange already embedded
// anywhere in the index is reused by its hash), sessions of a listed computer that are gone are dropped, and
// computers no longer listed are dropped entirely.
func (v *vectorIndex) refresh(ctx context.Context, sets []sessionSet) error {
	v.refreshMu.Lock()
	defer v.refreshMu.Unlock()

	v.mu.RLock()
	known := map[uint64]vecChunk{}
	for _, s := range v.sessions {
		for _, c := range s.Chunks {
			known[c.Hash] = c
		}
	}
	v.mu.RUnlock()

	type pending struct {
		key   string
		meta  *vecSession
		texts []chunkText
	}
	var todo []pending
	keep := map[string]bool{}
	machines := map[string]bool{}
	var missing []chunkText
	seenMissing := map[uint64]bool{}
	for _, set := range sets {
		machines[set.Machine] = true
		for _, s := range set.Sessions {
			key := vecKey(set.Machine, s.SessionID)
			keep[key] = true
			v.mu.RLock()
			old := v.sessions[key]
			v.mu.RUnlock()
			if old != nil && old.UpdatedAt.Equal(s.UpdatedAt) && old.Project == s.Project {
				continue
			}
			chunks := sessionChunks(s)
			for _, c := range chunks {
				if _, ok := known[c.Hash]; !ok && !seenMissing[c.Hash] {
					seenMissing[c.Hash] = true
					missing = append(missing, c)
				}
			}
			todo = append(todo, pending{key: key, texts: chunks, meta: &vecSession{
				Machine: set.Machine, SessionID: s.SessionID, Project: s.Project, CWD: s.CWD,
				Harness: s.Harness, Model: s.Model, UpdatedAt: s.UpdatedAt,
			}})
		}
	}

	// Embed what is new, newest sessions first, at most vecMaxPerRefresh per refresh for a remote endpoint.
	limit := len(missing)
	if _, isHash := v.emb.(hashEmbedder); !isHash {
		limit = min(limit, vecMaxPerRefresh)
	}
	var embedErr error
	for i := 0; i < limit; i += vecEmbedBatch {
		if err := ctx.Err(); err != nil {
			embedErr = err
			break
		}
		batch := missing[i:min(i+vecEmbedBatch, limit)]
		texts := make([]string, len(batch))
		for j, c := range batch {
			texts[j] = c.Text
		}
		vecs, err := v.emb.Embed(ctx, texts)
		if err != nil {
			embedErr = err
			break
		}
		for j, c := range batch {
			q, inv := quantize(vecs[j])
			known[c.Hash] = vecChunk{Hash: c.Hash, Vec: q, Inv: inv}
		}
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	for _, p := range todo {
		complete := true
		for _, c := range p.texts {
			k, ok := known[c.Hash]
			if !ok {
				complete = false
				continue
			}
			k.Line = c.Line
			p.meta.Chunks = append(p.meta.Chunks, k)
		}
		if !complete {
			// Not all of it is embedded yet: keep the update time zero so the next refresh finishes it.
			p.meta.UpdatedAt = time.Time{}
		}
		v.sessions[p.key] = p.meta
		v.dirty = true
	}
	for key, s := range v.sessions {
		if machines[s.Machine] && !keep[key] {
			delete(v.sessions, key)
			v.dirty = true
		}
	}
	for key, s := range v.sessions {
		if !machines[s.Machine] {
			delete(v.sessions, key)
			v.dirty = true
		}
	}
	v.builtAt = time.Now().UTC()
	v.lastErr = ""
	if embedErr != nil {
		v.lastErr = clipRunes(embedErr.Error(), 160)
	}
	return embedErr
}

func quantize(vec []float32) ([]int8, float32) {
	var mx float32
	for _, x := range vec {
		mx = max(mx, float32(math.Abs(float64(x))))
	}
	if mx == 0 {
		return make([]int8, len(vec)), 0
	}
	scale := 127 / mx
	out := make([]int8, len(vec))
	for i, x := range vec {
		out[i] = int8(math.Round(float64(x * scale)))
	}
	return out, 1 / scale
}

func dotQ(a []float32, b []int8, inv float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var s float32
	for i, x := range b {
		s += a[i] * float32(x)
	}
	return float64(s * inv)
}

// vecFilter is what a query's scan_sessions steps narrow the sessions to.
type vecFilter struct {
	machine, project, projectKey, harness, model string
	since                                        time.Time
}

func (f vecFilter) allows(s *vecSession, machineName func(string) string) bool {
	if f.project != "" && !strings.Contains(strings.ToLower(s.Project), f.project) && !strings.Contains(strings.ToLower(s.CWD), f.project) {
		return false
	}
	if f.projectKey != "" && projectKey(s.Project) != f.projectKey {
		return false
	}
	if f.harness != "" && !strings.Contains(strings.ToLower(s.Harness), f.harness) {
		return false
	}
	if f.model != "" && !strings.Contains(strings.ToLower(s.Model), f.model) {
		return false
	}
	if !f.since.IsZero() && s.UpdatedAt.Before(f.since) {
		return false
	}
	if f.machine != "" && !machineMatches(f.machine, s.Machine, machineName(s.Machine)) {
		return false
	}
	return true
}

// search is the sessions closest to the query vector, per computer: each session's best exchange, as a
// confidence in [0, 1] (the similarity in the embedder's band, scaled down by how far the best match of the
// query is from clearly related).
func (v *vectorIndex) search(qv []float32, f vecFilter, machineName func(string) string, allowMachine func(string) bool) (map[string]map[string]semHint, int) {
	type best struct {
		s    *vecSession
		sim  float64
		line int
	}
	var all []best
	scanned := 0
	v.mu.RLock()
	for _, s := range v.sessions {
		if (allowMachine != nil && !allowMachine(s.Machine)) || !f.allows(s, machineName) {
			continue
		}
		b := best{s: s, sim: -1}
		for _, c := range s.Chunks {
			scanned++
			if sim := dotQ(qv, c.Vec, c.Inv); sim > b.sim {
				b.sim, b.line = sim, c.Line
			}
		}
		if b.sim > 0 {
			all = append(all, b)
		}
	}
	v.mu.RUnlock()
	slices.SortFunc(all, func(a, b best) int { return cmp.Compare(b.sim, a.sim) })
	all = all[:min(len(all), vecTopSessions)]
	out := map[string]map[string]semHint{}
	lo, hi := v.emb.band()
	for _, b := range all {
		conf := math.Min(1, math.Max(0, (b.sim-lo)/(hi-lo)))
		if conf < vecMinConfidence {
			continue
		}
		m := out[b.s.Machine]
		if m == nil {
			m = map[string]semHint{}
			out[b.s.Machine] = m
		}
		m[b.s.SessionID] = semHint{SessionID: b.s.SessionID, Sim: math.Round(conf*1000) / 1000, Line: b.line}
	}
	return out, scanned
}

// semanticQuery is the query, mode and filters of a program's first scan_sessions step with a query.
func semanticQuery(prog SearchProgram, now time.Time) (query string, f vecFilter, ok bool) {
	for _, s := range prog.Steps {
		if s.Op != "scan_sessions" {
			continue
		}
		q := parseQueryTerms(s.Args["query"])
		if q.empty() || searchMode(s.Args["mode"]) == modeLexical {
			continue
		}
		f = vecFilter{
			machine:    strings.TrimSpace(s.Args["machine"]),
			project:    strings.ToLower(strings.TrimSpace(s.Args["project"])),
			projectKey: strings.TrimSpace(s.Args["project_key"]),
			harness:    strings.ToLower(strings.TrimSpace(s.Args["harness"])),
			model:      strings.ToLower(strings.TrimSpace(s.Args["model"])),
			since:      parseSince(s.Args["since"], now),
		}
		return strings.Join(q.Raw, " "), f, true
	}
	return "", vecFilter{}, false
}

// semanticResult is what the vector stage of one search found.
type semanticResult struct {
	hints   map[string]map[string]semHint // by machine ("" the hub's own computer), then session id
	trace   StepTrace
	enabled bool
}

// semanticHints runs the vector stage of prog: nil hints when the program has no query to embed or the
// embedder fails (the search then stays keyword-only, which the trace says).
func (v *vectorIndex) semanticHints(ctx context.Context, prog SearchProgram, now time.Time, machineName func(string) string, allowMachine func(string) bool) semanticResult {
	if v == nil {
		return semanticResult{}
	}
	query, f, ok := semanticQuery(prog, now)
	if !ok {
		return semanticResult{}
	}
	start := time.Now()
	st := v.stats()
	res := semanticResult{enabled: true, trace: StepTrace{
		Op:         "vector_search",
		Code:       fmt.Sprintf("vector_search(query=%q, embedder=%q, k=%d)", clipRunes(query, 40), embedderLabel(v.emb), vecTopSessions),
		InputCount: st.Sessions,
	}}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	vecs, err := v.emb.Embed(cctx, []string{query})
	cancel()
	if err == nil && (len(vecs) != 1 || len(vecs[0]) == 0) {
		err = errors.New("empty query vector")
	}
	if err != nil {
		res.trace.Note = "查询向量化失败，本次只用关键词检索：" + clipRunes(err.Error(), 80)
		res.trace.DurationUs = max(time.Since(start).Microseconds(), 1)
		return res
	}
	hints, scanned := v.search(vecs[0], f, machineName, allowMachine)
	n := 0
	for _, m := range hints {
		n += len(m)
	}
	res.hints = hints
	res.trace.OutputCount = n
	res.trace.DurationMs = time.Since(start).Milliseconds()
	res.trace.DurationUs = max(time.Since(start).Microseconds(), 1)
	res.trace.Note = fmt.Sprintf("hub 集中向量索引：%d 台电脑 %d 个会话的 %d 个对话片段，语义相近的 %d 个会话交给各电脑与关键词分数融合（关键词 %.0f%% + 语义 %.0f%%）",
		st.Machines, st.Sessions, scanned, n, lexWeight*100, semWeight*100)
	return res
}

func embedderLabel(e embedder) string {
	switch x := e.(type) {
	case hashEmbedder:
		return "hash"
	case *httpEmbedder:
		return x.model
	}
	return e.ID()
}

// hintList is a computer's hints as nodeSearchParams carries them.
func hintList(m map[string]semHint) []semHint {
	out := make([]semHint, 0, len(m))
	for _, h := range m {
		out = append(out, h)
	}
	slices.SortFunc(out, func(a, b semHint) int { return cmp.Compare(a.SessionID, b.SessionID) })
	return out
}

func hintMap(list []semHint) map[string]semHint {
	if list == nil {
		return nil
	}
	m := make(map[string]semHint, len(list))
	for _, h := range list {
		if h.SessionID != "" {
			h.Sim = math.Min(1, math.Max(0, h.Sim))
			m[h.SessionID] = h
		}
	}
	return m
}

// prependTrace puts t before the program's traces and numbers them again.
func prependTrace(res *ExecutionResult, t StepTrace) {
	res.StepTraces = append([]StepTrace{t}, res.StepTraces...)
	for i := range res.StepTraces {
		res.StepTraces[i].Index = i + 1
	}
}
