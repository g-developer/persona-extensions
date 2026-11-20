package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func hitIDs(res *ExecutionResult) []string {
	var out []string
	for _, h := range res.Hits {
		out = append(out, h.SessionID)
	}
	return out
}

func semanticSnippet(h *SearchHit) bool {
	return len(h.Snippets) > 0 && h.Snippets[0].Semantic
}

func TestHashEmbedderAndIndexPersistence(t *testing.T) {
	e := hashEmbedder{dim: hashDim}
	v, _ := e.Embed(context.Background(), []string{"reconnecting websockets", "websocket reconnect", "数据库连接超时", "数据库超时", "bake bread"})
	cos := func(a, b []float32) (s float64) {
		for i := range a {
			s += float64(a[i] * b[i])
		}
		return
	}
	if cos(v[0], v[1]) <= cos(v[0], v[4])+0.2 || cos(v[2], v[3]) <= cos(v[2], v[4])+0.2 {
		t.Fatalf("inflections and partial CJK must land close: %.2f %.2f / %.2f %.2f", cos(v[0], v[1]), cos(v[0], v[4]), cos(v[2], v[3]), cos(v[2], v[4]))
	}

	dir := t.TempDir()
	sess := &SessionRecord{SessionID: "s1", Project: "p", UpdatedAt: t0, Turns: []SessionTurn{
		{Line: 1, Role: "user", Text: "how do I reconnect the websocket"},
		{Line: 2, Role: "assistant", Text: "use exponential backoff"},
		{Line: 3, Role: "tool", IsTool: true, Text: "TOOL OUTPUT never embedded"},
		{Line: 4, Role: "user", Text: strings.Repeat("long exchange ", 300) + "CONCLUSION AT THE END"},
	}}
	chunks := sessionChunks(sess)
	if len(chunks) < 3 || chunks[0].Line != 1 || strings.Contains(chunks[0].Text, "TOOL OUTPUT") ||
		!strings.Contains(chunks[len(chunks)-1].Text, "CONCLUSION AT THE END") {
		t.Fatalf("exchanges, no tool output, the tail of a long one kept: %+v", chunks)
	}
	idx := openVectorIndex(dir, e)
	if err := idx.refresh(context.Background(), []sessionSet{{Sessions: []*SessionRecord{sess}}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.save(); err != nil {
		t.Fatal(err)
	}
	again := openVectorIndex(dir, e)
	if st := again.stats(); st.Sessions != 1 || st.Chunks != len(chunks) {
		t.Fatalf("index must persist: %+v", st)
	}
	// Another embedder never reads these vectors.
	if st := openVectorIndex(dir, hashEmbedder{dim: 64}).stats(); st.Sessions != 0 {
		t.Fatalf("vectors of another embedder: %+v", st)
	}
	// A computer no longer listed is dropped.
	if err := again.refresh(context.Background(), []sessionSet{{Machine: "m-x"}}); err != nil {
		t.Fatal(err)
	}
	if st := again.stats(); st.Sessions != 0 {
		t.Fatalf("stale computer kept: %+v", st)
	}
}

func TestHybridSearchFindsWhatKeywordsMiss(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "sess-ws", "/w/chat", "the websocket keeps reconnecting after deploys", "added jittered backoff", t0.Add(-time.Hour))
	writeSession(t, dir, "sess-other", "/w/bake", "bread recipe", "more flour", t0.Add(-time.Hour))
	a := newApp(t.TempDir())
	a.vec.configure(a.store.dir, semanticHash, "", "")
	st := &State{SessionDirs: []string{dir}}

	// "reconnects" is in no transcript: keywords alone find nothing.
	lex, _ := CompileProgram(CompileInput{Query: "websockets reconnects", Mode: modeLexical})
	if res := a.execute(context.Background(), st, lex, t0); len(res.Hits) != 0 || res.Semantic {
		t.Fatalf("lexical: %v", hitIDs(res))
	}
	prog, _ := CompileProgram(CompileInput{Query: "websockets reconnects"})
	res := a.execute(context.Background(), st, prog, t0)
	if len(res.Hits) != 1 || res.Hits[0].SessionID != "sess-ws" || !semanticSnippet(res.Hits[0]) || !res.Semantic {
		t.Fatalf("hybrid must find it through the vector index: %+v", res.Hits)
	}
	if res.StepTraces[0].Op != "vector_search" || res.StepTraces[0].Index != 1 || res.StepTraces[1].Op != "scan_sessions" {
		t.Fatalf("trace: %+v", res.StepTraces[:2])
	}
	// Vectors off: the same program is keyword-only and says nothing of vectors.
	b := newApp(t.TempDir())
	if res := b.execute(context.Background(), st, prog, t0); len(res.Hits) != 0 || res.StepTraces[0].Op != "scan_sessions" {
		t.Fatalf("vectors off: %v %+v", hitIDs(res), res.StepTraces[0])
	}
}

func TestCentralVectorsReachEveryComputer(t *testing.T) {
	tf := newTestFleet(t)
	ctx := context.Background()
	st := &State{SessionDirs: []string{tf.hubDir}}
	tf.f.syncAll(ctx)

	a := newApp(t.TempDir())
	a.fleet = tf.f
	a.vec.configure(a.store.dir, semanticHash, "", "")
	if err := a.vec.refresh(ctx, st, tf.f); err != nil {
		t.Fatal(err)
	}
	if vs := a.vec.index().stats(); vs.Machines != 3 {
		t.Fatalf("the hub indexes its own and every synced computer: %+v", vs)
	}

	// Online laptop: the hub hands its sessions over nodes.call; the laptop fuses them with its keyword scores.
	prog, _ := CompileProgram(CompileInput{Query: "cooldowns"})
	res := a.execute(ctx, st, prog, t0)
	var lap *SearchHit
	for _, h := range res.Hits {
		if h.SessionID == "sess-lap-1" {
			lap = h
		}
	}
	if lap == nil || lap.Machine != "m-1a2b" || lap.Cached || !semanticSnippet(lap) {
		t.Fatalf("laptop hit through central vectors: %+v", res.Hits)
	}

	// Offline desktop: searched semantically in its synced index all the same.
	tf.nodes.setDown("m-3c4d", true)
	prog, _ = CompileProgram(CompileInput{Query: "plannings machine:desktop"})
	res = a.execute(ctx, st, prog, t0)
	if len(res.Hits) != 1 || res.Hits[0].SessionID != "sess-desk-1" || !res.Hits[0].Cached || !semanticSnippet(res.Hits[0]) {
		t.Fatalf("offline desktop: %+v", res.Hits)
	}

	// A route names its computers: the vector stage never hands over sessions of another.
	targets, _ := resolveTargets(tf.f.targets(ctx), "laptop-zc")
	prog, _ = CompileProgram(CompileInput{Query: "plannings"})
	sem := a.vec.hints(ctx, st, tf.f, prog, t0, targets)
	for m := range sem.hints {
		if m != "m-1a2b" {
			t.Fatalf("hints for a computer outside the route: %v", sem.hints)
		}
	}
}

func TestNodeFusesHintsAndOldHubsStillWork(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "sess-a", "/w/a", "alpha question", "alpha answer", t0)
	w := newNodeWorker(t.TempDir(), configSpec{SessionDirs: dir})
	prog, _ := CompileProgram(CompileInput{Query: "zzz-not-there"})
	raw, _ := json.Marshal(nodeSearchParams{Program: prog, Machine: "m", Semantic: []semHint{{SessionID: "sess-a", Sim: 0.9, Line: 1}}})
	out, err := w.rpc(context.Background(), "search", raw)
	if err != nil {
		t.Fatal(err)
	}
	if res := out.(*ExecutionResult); len(res.Hits) != 1 || !semanticSnippet(res.Hits[0]) {
		t.Fatalf("a hinted session is a hit: %+v", res.Hits)
	}
	// The vector stage ran but found nothing here: keyword scores take the same hybrid weight as everywhere.
	kw, _ := CompileProgram(CompileInput{Query: "alpha"})
	raw, _ = json.Marshal(nodeSearchParams{Program: kw, Machine: "m"})
	out, _ = w.rpc(context.Background(), "search", raw)
	plain := out.(*ExecutionResult).Hits[0].Score
	raw, _ = json.Marshal(nodeSearchParams{Program: kw, Machine: "m", Fused: true})
	out, _ = w.rpc(context.Background(), "search", raw)
	if fusedScore := out.(*ExecutionResult).Hits[0].Score; fusedScore >= plain {
		t.Fatalf("a fused search weights keywords on every computer alike: %v vs %v", fusedScore, plain)
	}
	raw, _ = json.Marshal(nodeSearchParams{Program: prog, Machine: "m"}) // a hub without vectors
	out, _ = w.rpc(context.Background(), "search", raw)
	if res := out.(*ExecutionResult); len(res.Hits) != 0 {
		t.Fatalf("without hints the search is keyword-only: %+v", res.Hits)
	}
}

func TestEndpointEmbedder(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var in struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if r.URL.Path != "/v1/embeddings" || json.NewDecoder(r.Body).Decode(&in) != nil || in.Model != "bge-m3" {
			http.Error(w, "bad", 400)
			return
		}
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var data []item
		for i, s := range in.Input {
			v := []float32{0, 0, 0}
			switch {
			case strings.Contains(s, "超时") || strings.Contains(s, "timeout"):
				v[0] = 1 // a "multilingual" model: both languages, one direction
			default:
				v[2] = 1
			}
			data = append(data, item{Index: i, Embedding: v})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()

	dir := t.TempDir()
	writeSession(t, dir, "sess-en", "/w/en", "the request hits a timeout under load", "raise the pool size", t0)
	writeSession(t, dir, "sess-x", "/w/x", "unrelated", "nothing", t0)
	a := newApp(t.TempDir())
	a.vec.configure(a.store.dir, semanticEndpoint, srv.URL+"/v1/", "bge-m3")
	st := &State{SessionDirs: []string{dir}}
	if err := a.vec.refresh(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	n := calls.Load()
	if err := a.vec.refresh(context.Background(), st, nil); err != nil || calls.Load() != n {
		t.Fatal("unchanged sessions are not embedded again")
	}
	prog, _ := CompileProgram(CompileInput{Query: "请求超时"})
	res := a.execute(context.Background(), st, prog, t0)
	if len(res.Hits) != 1 || res.Hits[0].SessionID != "sess-en" {
		t.Fatalf("a Chinese question finds the English session: %+v", res.Hits)
	}

	// The endpoint fails: the search stays keyword-only and the trace says why.
	srv.Close()
	res = a.execute(context.Background(), st, prog, t0)
	if len(res.Hits) != 0 || !strings.Contains(res.StepTraces[0].Note, "只用关键词") {
		t.Fatalf("fallback: %+v %+v", res.Hits, res.StepTraces[0])
	}
}

func TestMCPUsesTheHubIndexReadOnly(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "sess-ws", "/w/chat", "the websocket keeps reconnecting", "backoff", t0)
	data := t.TempDir()
	a := newApp(data)
	a.vec.configure(data, semanticHash, "", "")
	st := &State{SessionDirs: []string{dir}, Semantic: semanticHash}
	if err := a.vec.refresh(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.store.update(func(s *State, _ time.Time) error {
		s.SessionDirs, s.Semantic = []string{dir}, semanticHash
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(vectorPath(data, hashEmbedder{dim: hashDim}))
	m := &mcpServer{store: newStore(data)}
	out, err := m.call("sac_session_search", map[string]any{"query": "websockets reconnects"})
	if err != nil {
		t.Fatal(err)
	}
	if res := out.(*ExecutionResult); len(res.Hits) != 1 || !semanticSnippet(res.Hits[0]) {
		t.Fatalf("mcp hybrid: %+v", res.Hits)
	}
	if fi2, _ := os.Stat(vectorPath(data, hashEmbedder{dim: hashDim})); !fi2.ModTime().Equal(fi.ModTime()) {
		t.Fatal("the MCP server must not write the hub's index")
	}
}
