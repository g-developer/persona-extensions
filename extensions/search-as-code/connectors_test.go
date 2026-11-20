package main

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Synthetic stores of the other coding agents, laid out as each keeps them (connectors.go).

func mkdb(t *testing.T, path string, stmts ...string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return db
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func withAgentHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	old := connectorHome
	connectorHome = func() string { return home }
	t.Cleanup(func() { connectorHome = old })
	return home
}

func bySession(recs []*SessionRecord) map[string]*SessionRecord {
	m := map[string]*SessionRecord{}
	for _, r := range recs {
		m[r.SessionID] = r
	}
	return m
}

func TestCursorEditorComposersAndBubbles(t *testing.T) {
	home := withAgentHome(t)
	user := cursorUserDir(home)
	ms := t0.Add(-2 * time.Hour).UnixMilli()

	db := mkdb(t, filepath.Join(user, "globalStorage", "state.vscdb"),
		`CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`)
	put := func(k string, v any) {
		if _, err := db.Exec(`INSERT INTO cursorDiskKV VALUES (?, ?)`, k, jsonOf(t, v)); err != nil {
			t.Fatal(err)
		}
	}
	put("composerData:comp-new", map[string]any{
		"composerId": "comp-new", "name": "Fix the flaky websocket reconnect", "createdAt": ms, "lastUpdatedAt": ms + 1000,
		"modelConfig":                 map[string]any{"modelName": "gpt-5"},
		"fullConversationHeadersOnly": []map[string]any{{"bubbleId": "b1", "type": 1}, {"bubbleId": "b2", "type": 2}, {"bubbleId": "b3", "type": 2}},
	})
	put("bubbleId:comp-new:b1", map[string]any{"type": 1, "text": "The websocket reconnect loop in src/net/socket.ts drops messages", "createdAt": "2026-11-09T08:00:00Z"})
	put("bubbleId:comp-new:b2", map[string]any{"type": 2, "text": "", "toolFormerData": map[string]any{"name": "run_terminal_cmd", "rawArgs": `{"command":"npm test"}`, "result": "Error: ECONNRESET in reconnect.spec.ts", "status": "error"}})
	put("bubbleId:comp-new:b3", map[string]any{"type": 2, "text": "Backoff now resets after a successful handshake."})
	// An older composer that keeps its conversation inline.
	put("composerData:comp-old", map[string]any{
		"composerId": "comp-old", "createdAt": ms - 86400000,
		"conversation": []map[string]any{{"type": 1, "text": "rename the GRAPHQUILL resolver"}, {"type": 2, "text": "Renamed it."}},
	})
	put("composerData:comp-empty", map[string]any{"composerId": "comp-empty", "createdAt": ms})

	// The workspace the new composer belongs to.
	ws := filepath.Join(user, "workspaceStorage", "abc123")
	writeFile(t, filepath.Join(ws, "workspace.json"), `{"folder":"file:///Users/me/src/chat-app"}`)
	wdb := mkdb(t, filepath.Join(ws, "state.vscdb"), `CREATE TABLE ItemTable (key TEXT UNIQUE, value BLOB)`)
	if _, err := wdb.Exec(`INSERT INTO ItemTable VALUES ('composer.composerData', ?)`, `{"allComposers":[{"composerId":"comp-new"}]}`); err != nil {
		t.Fatal(err)
	}

	got := bySession(cursorSessions(home))
	if len(got) != 2 {
		t.Fatalf("want the two composers with messages, got %v", got)
	}
	s := got["comp-new"]
	if s.Harness != "cursor" || s.Project != "chat-app" || s.Model != "gpt-5" || s.Title != "Fix the flaky websocket reconnect" {
		t.Fatalf("composer fields: %+v", s)
	}
	if len(s.Turns) != 3 || s.Turns[0].Role != "user" || !s.Turns[1].IsTool || !s.Turns[1].IsError || s.Turns[2].Role != "assistant" {
		t.Fatalf("turns: %+v", s.Turns)
	}
	if !slices.Contains(s.Files, "src/net/socket.ts") || s.ErrorCount != 1 {
		t.Fatalf("files / errors: %+v %d", s.Files, s.ErrorCount)
	}
	if o := got["comp-old"]; o.Project != "cursor" || o.Title != "rename the GRAPHQUILL resolver" {
		t.Fatalf("inline composer: %+v", o)
	}

	// Searchable like any other session, and filtered by harness.
	st := &State{}
	prog, err := CompileProgram(CompileInput{Query: "websocket reconnect harness:cursor", Scope: "conversation"})
	if err != nil {
		t.Fatal(err)
	}
	res := ExecuteProgram(st, prog, t0)
	if len(res.Hits) == 0 || res.Hits[0].SessionID != "comp-new" {
		t.Fatalf("cursor hit: %+v", res.Hits)
	}
}

func TestCursorAgentTranscriptsAndCLI(t *testing.T) {
	home := withAgentHome(t)
	writeFile(t, filepath.Join(home, ".cursor", "projects", "Users-me-src-billing", "agent-transcripts", "t1", "t1.jsonl"),
		`{"role":"user","message":{"content":[{"type":"text","text":"why does the INVOICEFOX export time out"}]}}`+"\n"+
			`{"role":"assistant","message":{"content":[{"type":"text","text":"The export query lacks an index."}]}}`+"\n")

	// Cursor CLI: meta row '0' (hex JSON) and blobs linked from the root.
	folder := "/Users/me/src/billing"
	path := filepath.Join(home, ".cursor", "chats", cursorCLIFolderHash(folder), "chat-1", "store.db")
	user := jsonOf(t, map[string]any{"role": "user", "content": "<user_info>os: darwin</user_info><user_query>add retries to the LEDGERWISP sync</user_query>"})
	asst := jsonOf(t, map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "Added exponential retries."}, {"type": "tool-call", "toolName": "edit_file", "args": map[string]any{"path": "sync/ledger.go"}}}})
	idU, idA := strings.Repeat("aa", 32), strings.Repeat("bb", 32)
	root := strings.Repeat("cc", 32)
	link := func(ids ...string) []byte {
		var b []byte
		for _, id := range ids {
			raw, _ := hex.DecodeString(id)
			b = append(append(b, 0x0A, 0x20), raw...)
		}
		return b
	}
	meta := hex.EncodeToString([]byte(jsonOf(t, map[string]any{"agentId": "agent-77", "latestRootBlobId": root, "name": "New Agent", "createdAt": t0.Add(-time.Hour).UnixMilli(), "lastUsedModel": "sonnet-5"})))
	db := mkdb(t, path, `CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT)`, `CREATE TABLE blobs (id TEXT PRIMARY KEY, data BLOB)`)
	for _, row := range [][2]any{{idU, []byte(user)}, {idA, []byte(asst)}, {root, link(idU, idA)}} {
		if _, err := db.Exec(`INSERT INTO blobs VALUES (?, ?)`, row[0], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO meta VALUES ('0', ?)`, meta); err != nil {
		t.Fatal(err)
	}

	got := bySession(loadConnectorSessions(nil))
	tr := got["t1"]
	if tr == nil || tr.Harness != "cursor" || tr.Project != "billing" || len(tr.Turns) != 2 || tr.Turns[0].Role != "user" {
		t.Fatalf("agent transcript: %+v", tr)
	}
	cli := got["agent-77"]
	if cli == nil || cli.Harness != "cursor-cli" || cli.Model != "sonnet-5" {
		t.Fatalf("cli session: %+v", cli)
	}
	if cli.Title != "add retries to the LEDGERWISP sync" || len(cli.Turns) != 3 || !cli.Turns[2].IsTool {
		t.Fatalf("cli turns: %q %+v", cli.Title, cli.Turns)
	}
}

func TestAntigravityAndGeminiCLI(t *testing.T) {
	home := withAgentHome(t)
	brain := filepath.Join(home, ".gemini", "antigravity", "brain", "f1e2d3c4")
	steps := []map[string]any{
		{"step_index": 0, "source": "USER_EXPLICIT", "type": "USER_INPUT", "created_at": "2026-11-09T07:00:00Z",
			"content": "<USER_REQUEST>\nMigrate the PELICANDB schema\n</USER_REQUEST>\n<USER_SETTINGS_CHANGE>\nThe user changed setting `Model Selection` from None to Gemini 3.1 Pro (High). No need to comment.\n</USER_SETTINGS_CHANGE>"},
		{"step_index": 1, "source": "SYSTEM", "type": "EPHEMERAL_MESSAGE", "content": "Always prefer the most specific tool."},
		{"step_index": 2, "source": "MODEL", "type": "PLANNER_RESPONSE", "created_at": "2026-11-09T07:00:05Z", "content": "I'll write a migration plan first."},
		{"step_index": 3, "source": "MODEL", "type": "RUN_COMMAND", "status": "DONE", "content": "exit status 1: relation missing", "tool_calls": []map[string]any{{"name": "run_command", "args": map[string]any{"Cwd": "/home/me/pelican"}}}},
	}
	var lines []string
	for _, s := range steps {
		lines = append(lines, jsonOf(t, s))
	}
	writeFile(t, filepath.Join(brain, ".system_generated", "logs", "transcript.jsonl"), strings.Join(lines, "\n")+"\n")
	writeFile(t, filepath.Join(brain, "implementation_plan.md"), "# Plan\nAdd a NOT NULL default before backfilling.")

	gem := filepath.Join(home, ".gemini", "tmp", "hash1", "chats")
	writeFile(t, filepath.Join(gem, "session-2026-11-09.json"), jsonOf(t, map[string]any{
		"sessionId": "gem-1", "lastUpdated": "2026-11-09T09:00:00Z",
		"messages": []map[string]any{
			{"type": "user", "content": "explain the KESTRELMQ consumer lag"},
			{"type": "gemini", "model": "gemini-3-pro", "content": "Lag grows because acks are batched.", "toolCalls": []map[string]any{{"name": "read_file", "status": "success", "args": map[string]any{"dir_path": "/home/me/kestrel"}, "result": "ok"}}},
		},
	}))
	writeFile(t, filepath.Join(gem, "session-2.jsonl"),
		`{"sessionId":"gem-2","projectHash":"hash1","startTime":"2026-11-09T06:00:00Z"}`+"\n"+
			`{"id":"m1","type":"user","content":"first draft"}`+"\n"+
			`{"id":"m1","type":"user","content":"tune the OSPREY cache"}`+"\n"+
			`{"id":"m2","type":"gemini","content":[{"text":"Raised the TTL."}]}`+"\n")

	got := bySession(loadConnectorSessions(nil))
	agy := got["f1e2d3c4"]
	if agy == nil || agy.Harness != "antigravity" || agy.Model != "Gemini 3.1 Pro (High)" || agy.Project != "pelican" {
		t.Fatalf("antigravity: %+v", agy)
	}
	roles := []string{}
	for _, tr := range agy.Turns {
		roles = append(roles, tr.Role)
	}
	if !slices.Equal(roles, []string{"user", "assistant", "error", "assistant"}) || agy.Title != "Migrate the PELICANDB schema" {
		t.Fatalf("antigravity turns %v title %q", roles, agy.Title)
	}
	if g := got["gem-1"]; g == nil || g.Harness != "gemini-cli" || g.Model != "gemini-3-pro" || g.Project != "kestrel" || len(g.Turns) != 3 {
		t.Fatalf("gemini json: %+v", g)
	}
	if g := got["gem-2"]; g == nil || len(g.Turns) != 2 || g.Turns[0].Text != "tune the OSPREY cache" {
		t.Fatalf("gemini jsonl (upsert by id): %+v", g)
	}

	// The plan is searchable as the agent's own words.
	prog, _ := CompileProgram(CompileInput{Query: "backfilling", Scope: "assistant"})
	res := ExecuteProgram(&State{}, prog, t0)
	if len(res.Hits) != 1 || res.Hits[0].SessionID != "f1e2d3c4" {
		t.Fatalf("plan hit: %+v", res.Hits)
	}
}

func TestParsedSessionsAreCachedUntilTheFileChanges(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "sess-cache", "/work/cachy", "first QUOKKA question", "answer", t0)
	st := &State{SessionDirs: []string{dir}}
	a, _ := loadSessions(st)
	b, _ := loadSessions(st)
	if len(a) != 1 || len(b) != 1 || &a[0].Turns[0] != &b[0].Turns[0] {
		t.Fatal("an unchanged transcript must be reused, not parsed again")
	}
	a[0].Machine = "elsewhere" // a caller's change stays its own
	if c, _ := loadSessions(st); c[0].Machine != "" {
		t.Fatal("cached record was changed through a copy")
	}
	writeSession(t, dir, "sess-cache", "/work/cachy", "second WOMBAT question", "answer", t0.Add(time.Minute))
	c, _ := loadSessions(st)
	if !strings.Contains(c[0].Turns[0].Text, "WOMBAT") {
		t.Fatalf("a changed transcript must be parsed again: %+v", c[0].Turns[0])
	}
}

func TestLexicalRelevance(t *testing.T) {
	q := parseQueryTerms(`数据库超时 "exit status 2" -flaky go project:x`)
	if !slices.Equal(q.Terms, []string{"数据", "据库", "库超", "超时", "exit status 2", "go"}) || !slices.Equal(q.Negative, []string{"flaky"}) {
		t.Fatalf("terms %q negative %q", q.Terms, q.Negative)
	}
	if termFreq("google go-to go", "go") != 2 || termFreq("nodes.call nodes", "nodes.call") != 1 {
		t.Fatal("short terms count at word boundaries only")
	}

	dir := t.TempDir()
	writeSession(t, dir, "strong", "/w/a", "数据库连接超时 after deploy", "raised the pool timeout for 数据库", t0.Add(-48*time.Hour))
	writeSession(t, dir, "weak", "/w/b", "超时 in an unrelated test", "ok", t0.Add(-1*time.Hour))
	writeSession(t, dir, "excluded", "/w/c", "数据库超时 again but flaky", "retry", t0.Add(-1*time.Hour))
	st := &State{SessionDirs: []string{dir}}
	prog, _ := CompileProgram(CompileInput{Query: "数据库超时 -flaky", Scope: "conversation"})
	res := ExecuteProgram(st, prog, t0)
	var ids []string
	for _, h := range res.Hits {
		ids = append(ids, h.SessionID)
	}
	if len(ids) == 0 || ids[0] != "strong" || slices.Contains(ids, "excluded") {
		t.Fatalf("BM25 must rank the session covering the query first and drop -flaky: %v", ids)
	}
	for _, h := range res.Hits {
		if h.Score > 10*1.2+4+0.01 {
			t.Fatalf("lexical relevance is normalized: %v", h.Score)
		}
	}
	if _, err := CompileProgram(CompileInput{Code: `scan_sessions(query="x", mode="fuzzy")`}); err == nil {
		t.Fatal("an unknown mode must be refused")
	}
	if p, _ := CompileProgram(CompileInput{Query: "x mode:semantic"}); !strings.Contains(p.CodeListing, `mode="semantic"`) {
		t.Fatalf("mode token: %s", p.CodeListing)
	}
}
