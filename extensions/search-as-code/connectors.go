package main

import (
	"bufio"
	"cmp"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure Go (no CGO): Cursor keeps its history in SQLite
)

// Connectors for coding agents whose history is not a Claude Code / Codex style JSONL tree (the formats as
// CASS, github.com/Dicklesworthstone/coding_agent_session_search, documents and reads them):
//
//   - Cursor (harness "cursor"): the editor's chats and agent (composer) sessions in
//     <Cursor user dir>/globalStorage/state.vscdb, table cursorDiskKV: composerData:<id> (name, createdAt,
//     lastUpdatedAt, modelConfig, fullConversationHeadersOnly[].bubbleId, or the older inline conversation[])
//     and bubbleId:<composerId>:<bubbleId> (type 1 user / 2 assistant, text, toolFormerData). The workspace
//     folder of a composer comes from workspaceStorage/<id>/workspace.json and that workspace's
//     composer.composerData. Newer agent transcripts in ~/.cursor/projects/<project>/agent-transcripts are
//     JSONL and read like Claude Code's.
//   - Cursor CLI (harness "cursor-cli"): ~/.cursor/chats/<md5 of the folder>/<id>/store.db, table meta (key
//     '0': hex JSON with name, createdAt, lastUsedModel, latestRootBlobId) and blobs (JSON messages linked
//     from the root blob).
//   - Antigravity (harness "antigravity"): ~/.gemini/antigravity{,-cli}/brain/<id>/.system_generated/logs/
//     transcript.jsonl (steps: USER_INPUT, PLANNER_RESPONSE, tool steps) and the brain's markdown artifacts
//     (task.md, implementation_plan.md, walkthrough.md) as assistant turns. Its conversations/<id>.pb files
//     are encrypted and its conversations/<id>.db hold protobuf steps; neither is read.
//   - Gemini CLI (harness "gemini-cli"): ~/.gemini/tmp/<project hash>/chats/session-*.json(l).
//
// Every database is opened read-only; a source is parsed again only when its files change (and a busy Cursor
// database at most once a minute).

// connectorHome is the home directory the connectors look in ("" turns them off; tests point it elsewhere).
var connectorHome = func() string {
	if env := strings.TrimSpace(os.Getenv("PERSONA_SEARCH_AS_CODE_AGENT_HOME")); env != "" {
		return env
	}
	h, _ := os.UserHomeDir()
	return h
}

const (
	maxConnectorSessions = 600 // per connector, newest first
	cursorRescanEvery    = time.Minute
)

// harnessLabels names the supported coding agents.
var harnessLabels = map[string]string{
	"claude-code": "Claude Code",
	"codex":       "Codex",
	"cursor":      "Cursor",
	"cursor-cli":  "Cursor CLI",
	"antigravity": "Antigravity",
	"gemini-cli":  "Gemini CLI",
}

// loadConnectorSessions reads every connector's sessions on this computer.
func loadConnectorSessions(_ *State) []*SessionRecord {
	home := connectorHome()
	if home == "" {
		return nil
	}
	var out []*SessionRecord
	out = append(out, cursorSessions(home)...)
	out = append(out, cursorCLISessions(home)...)
	out = append(out, antigravitySessions(home)...)
	out = append(out, geminiCLISessions(home)...)
	return out
}

// connectorSources lists the connector locations that exist on this computer (the sources tab).
func connectorSources() []string {
	home := connectorHome()
	if home == "" {
		return nil
	}
	var out []string
	cands := []string{filepath.Join(cursorUserDir(home), "globalStorage", "state.vscdb"),
		filepath.Join(home, ".cursor", "projects"), filepath.Join(home, ".cursor", "chats"),
		filepath.Join(home, ".gemini", "antigravity", "brain"), filepath.Join(home, ".gemini", "antigravity-cli", "brain"),
		filepath.Join(home, ".gemini", "tmp")}
	for _, c := range cands {
		if exists(c) {
			out = append(out, c)
		}
	}
	return out
}

func newestFirst(recs []*SessionRecord) []*SessionRecord {
	recs = slices.DeleteFunc(recs, func(r *SessionRecord) bool { return r == nil || len(r.Turns) == 0 })
	slices.SortFunc(recs, func(a, b *SessionRecord) int { return b.UpdatedAt.Compare(a.UpdatedAt) })
	return recs[:min(len(recs), maxConnectorSessions)]
}

// finishRecord fills what every parser derives the same way: counts, title, project and files.
func finishRecord(rec *SessionRecord) *SessionRecord {
	var files []string
	rec.TurnCount, rec.ToolCalls, rec.ErrorCount = 0, 0, 0
	for i := range rec.Turns {
		tr := &rec.Turns[i]
		if tr.Line == 0 {
			tr.Line = i + 1
		}
		if tr.IsTool {
			rec.ToolCalls++
		} else {
			rec.TurnCount++
		}
		if tr.IsError {
			rec.ErrorCount++
		}
		if tr.At.After(rec.UpdatedAt) {
			rec.UpdatedAt = tr.At
		}
		tr.Files = extractFilesFromText(tr.Text)
		for _, f := range tr.Files {
			if !slices.Contains(files, f) && len(files) < 24 {
				files = append(files, f)
			}
		}
	}
	rec.Files = files
	if rec.Project == "" {
		rec.Project = rec.Harness
		if rec.CWD != "" {
			rec.Project = inferProject(rec.CWD, "")
		}
	}
	if strings.TrimSpace(rec.Title) == "" {
		rec.Title = conversationTitle(rec.Turns, rec.SessionID)
	}
	rec.Title = firstLineTitle(rec.Title)
	return rec
}

func convTurn(role, text string, at time.Time) (SessionTurn, bool) {
	text = cleanTurnText(text)
	if text == "" {
		return SessionTurn{}, false
	}
	isErr := isConversationalError(text, false)
	return SessionTurn{Role: role, Text: text, IsError: isErr, ErrorKind: condErrorKind(isErr, text), At: at}, true
}

func toolTurn(name, text string, explicitErr bool, at time.Time) (SessionTurn, bool) {
	text = cleanToolText(cmp.Or(strings.TrimSpace(text), name))
	if text == "" {
		return SessionTurn{}, false
	}
	isErr := isErrorTurn(text, explicitErr)
	role := "tool"
	if isErr {
		role = "error"
	}
	return SessionTurn{Role: role, Text: text, ToolName: name, IsTool: true, IsError: isErr, ErrorKind: condErrorKind(isErr, text), At: at}, true
}

func msTime(ms float64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	if ms < 1e11 { // seconds
		return time.Unix(int64(ms), 0).UTC()
	}
	return time.UnixMilli(int64(ms)).UTC()
}

func anyTime(v any) time.Time {
	switch x := v.(type) {
	case float64:
		return msTime(x)
	case string:
		return parseRFC3339(x)
	}
	return time.Time{}
}

// ---- SQLite ----

// openReadOnly opens a SQLite database without writing to it (a busy editor keeps it open in WAL mode).
func openReadOnly(path string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// fileStamp is a database and its write-ahead log as one change marker.
func fileStamp(path string) (string, bool) {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	s := fmt.Sprintf("%d/%d", fi.Size(), fi.ModTime().UnixNano())
	if wi, err := os.Stat(path + "-wal"); err == nil {
		s += fmt.Sprintf("+%d/%d", wi.Size(), wi.ModTime().UnixNano())
	}
	return s, true
}

// stampCache keeps a parsed source until its stamp changes (and, with every set, at most once per interval).
type stampCache struct {
	mu sync.Mutex
	m  map[string]stampEntry
}

type stampEntry struct {
	stamp string
	at    time.Time
	recs  []*SessionRecord
}

var connCache = &stampCache{m: map[string]stampEntry{}}

func (c *stampCache) get(key, stamp string, every time.Duration, parse func() []*SessionRecord) []*SessionRecord {
	c.mu.Lock()
	e, ok := c.m[key]
	c.mu.Unlock()
	if !ok || (e.stamp != stamp && time.Since(e.at) >= every) {
		e = stampEntry{stamp: stamp, at: time.Now(), recs: parse()}
		c.mu.Lock()
		c.m[key] = e
		c.mu.Unlock()
	}
	out := make([]*SessionRecord, len(e.recs))
	for i, r := range e.recs {
		out[i] = copyRecord(r)
	}
	return out
}

// ---- Cursor (editor) ----

func cursorUserDir(home string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "Cursor", "User")
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" && home == mustHome() {
		return filepath.Join(x, "Cursor", "User")
	}
	return filepath.Join(home, ".config", "Cursor", "User")
}

func mustHome() string {
	h, _ := os.UserHomeDir()
	return h
}

func cursorSessions(home string) []*SessionRecord {
	user := cursorUserDir(home)
	global := filepath.Join(user, "globalStorage", "state.vscdb")
	var out []*SessionRecord
	if stamp, ok := fileStamp(global); ok {
		out = append(out, connCache.get("cursor:"+global, stamp, cursorRescanEvery, func() []*SessionRecord {
			return parseCursorGlobal(global, cursorWorkspaces(filepath.Join(user, "workspaceStorage")))
		})...)
	}
	// Cursor agent transcripts (Claude Code shaped JSONL).
	root := filepath.Join(home, ".cursor", "projects")
	var recs []*SessionRecord
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") || !strings.Contains(filepath.ToSlash(path), "/agent-transcripts/") {
			return nil
		}
		fi, err := d.Info()
		if err != nil || fi.Size() == 0 {
			return nil
		}
		rec := cachedParseFile(fileEntry{path: path, mod: fi.ModTime(), size: fi.Size()})
		if rec != nil {
			rec.Harness = "cursor"
			if rec.Model == "claude-sonnet-5-5" { // parseSessionFile's guess for an unknown model
				rec.Model = ""
			}
			if rec.CWD == "" {
				rec.Project = cursorProjectName(path, root)
			}
			recs = append(recs, rec)
		}
		return nil
	})
	return newestFirst(append(out, recs...))
}

// cursorProjectName is the project folder name Cursor encodes in ~/.cursor/projects/<Users-me-src-app>.
func cursorProjectName(path, root string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "cursor"
	}
	enc := strings.Split(filepath.ToSlash(rel), "/")[0]
	parts := strings.Split(strings.Trim(enc, "-"), "-")
	return cmp.Or(parts[len(parts)-1], "cursor")
}

// cursorWorkspaces maps composer ids to the folder of the workspace they belong to.
func cursorWorkspaces(dir string) map[string]string {
	out := map[string]string{}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name(), "workspace.json"))
		if err != nil {
			continue
		}
		var ws struct {
			Folder string `json:"folder"`
		}
		if json.Unmarshal(raw, &ws) != nil || ws.Folder == "" {
			continue
		}
		folder := ws.Folder
		if u, err := url.Parse(ws.Folder); err == nil && u.Scheme == "file" {
			folder = u.Path
		}
		db, err := openReadOnly(filepath.Join(dir, e.Name(), "state.vscdb"))
		if err != nil {
			continue
		}
		var val string
		if db.QueryRow(`SELECT value FROM ItemTable WHERE key = 'composer.composerData'`).Scan(&val) == nil {
			var cd struct {
				AllComposers []struct {
					ComposerID string `json:"composerId"`
				} `json:"allComposers"`
			}
			if json.Unmarshal([]byte(val), &cd) == nil {
				for _, c := range cd.AllComposers {
					out[c.ComposerID] = folder
				}
			}
		}
		db.Close()
	}
	return out
}

type cursorComposer struct {
	ComposerID    string  `json:"composerId"`
	Name          string  `json:"name"`
	CreatedAt     float64 `json:"createdAt"`
	LastUpdatedAt float64 `json:"lastUpdatedAt"`
	ModelConfig   struct {
		ModelName string `json:"modelName"`
	} `json:"modelConfig"`
	Headers []struct {
		BubbleID string `json:"bubbleId"`
	} `json:"fullConversationHeadersOnly"`
	Conversation []json.RawMessage `json:"conversation"`
}

type cursorBubble struct {
	BubbleID  string  `json:"bubbleId"`
	Type      any     `json:"type"`
	Role      string  `json:"role"`
	Text      string  `json:"text"`
	RawText   string  `json:"rawText"`
	CreatedAt any     `json:"createdAt"`
	Timestamp float64 `json:"timestamp"`
	ModelType string  `json:"modelType"`
	ToolForm  *struct {
		Name    string `json:"name"`
		RawArgs string `json:"rawArgs"`
		Result  string `json:"result"`
		Status  string `json:"status"`
	} `json:"toolFormerData"`
	ModelInfo *struct {
		ModelName string `json:"modelName"`
	} `json:"modelInfo"`
}

func (b cursorBubble) role() string {
	switch t := b.Type.(type) {
	case float64:
		if t == 1 {
			return "user"
		}
		return "assistant"
	case string:
		if r := normalizeRole(t); r != "" {
			return r
		}
	}
	return cmp.Or(normalizeRole(b.Role), "assistant")
}

func parseCursorGlobal(path string, workspaces map[string]string) []*SessionRecord {
	db, err := openReadOnly(path)
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, err := db.Query(`SELECT key, value FROM cursorDiskKV WHERE key >= 'composerData:' AND key < 'composerData;' AND value IS NOT NULL`)
	if err != nil {
		return nil
	}
	var comps []cursorComposer
	for rows.Next() {
		var key string
		var val []byte
		if rows.Scan(&key, &val) != nil {
			continue
		}
		var c cursorComposer
		if json.Unmarshal(val, &c) != nil {
			continue
		}
		c.ComposerID = cmp.Or(c.ComposerID, strings.TrimPrefix(key, "composerData:"))
		comps = append(comps, c)
	}
	rows.Close()
	sort.Slice(comps, func(i, j int) bool {
		return max(comps[i].LastUpdatedAt, comps[i].CreatedAt) > max(comps[j].LastUpdatedAt, comps[j].CreatedAt)
	})
	comps = comps[:min(len(comps), maxConnectorSessions)]

	var out []*SessionRecord
	for _, c := range comps {
		var bubbles []cursorBubble
		if len(c.Headers) > 0 {
			byID := map[string]cursorBubble{}
			prefix := "bubbleId:" + c.ComposerID + ":"
			brows, err := db.Query(`SELECT key, value FROM cursorDiskKV WHERE key >= ? AND key < ? AND value IS NOT NULL`, prefix, "bubbleId:"+c.ComposerID+";")
			if err == nil {
				for brows.Next() {
					var key string
					var val []byte
					if brows.Scan(&key, &val) != nil {
						continue
					}
					var b cursorBubble
					if json.Unmarshal(val, &b) == nil {
						byID[strings.TrimPrefix(key, prefix)] = b
					}
				}
				brows.Close()
			}
			for _, h := range c.Headers {
				if b, ok := byID[h.BubbleID]; ok {
					bubbles = append(bubbles, b)
				}
			}
		} else {
			for _, raw := range c.Conversation {
				var b cursorBubble
				if json.Unmarshal(raw, &b) == nil {
					bubbles = append(bubbles, b)
				}
			}
		}
		rec := &SessionRecord{
			SessionID: c.ComposerID,
			Title:     c.Name,
			Harness:   "cursor",
			Model:     c.ModelConfig.ModelName,
			CWD:       workspaces[c.ComposerID],
			FilePath:  path,
			UpdatedAt: msTime(max(c.LastUpdatedAt, c.CreatedAt)),
		}
		if rec.Model == "default" {
			rec.Model = ""
		}
		for _, b := range bubbles {
			at := cmp.Or(anyTime(b.CreatedAt), msTime(b.Timestamp))
			if rec.Model == "" {
				rec.Model = cmp.Or(b.ModelType, func() string {
					if b.ModelInfo != nil {
						return b.ModelInfo.ModelName
					}
					return ""
				}())
			}
			if t := cmp.Or(b.Text, b.RawText); strings.TrimSpace(t) != "" {
				if tr, ok := convTurn(b.role(), t, at); ok {
					rec.Turns = append(rec.Turns, tr)
				}
			}
			if b.ToolForm != nil && b.ToolForm.Name != "" {
				txt := strings.TrimSpace(b.ToolForm.Name + " " + clipRunes(b.ToolForm.RawArgs, 300) + "\n" + b.ToolForm.Result)
				if tr, ok := toolTurn(b.ToolForm.Name, txt, b.ToolForm.Status == "error", at); ok {
					rec.Turns = append(rec.Turns, tr)
				}
			}
		}
		out = append(out, finishRecord(rec))
	}
	return out
}

// ---- Cursor CLI ----

func cursorCLISessions(home string) []*SessionRecord {
	root := filepath.Join(home, ".cursor", "chats")
	var out []*SessionRecord
	projects, _ := os.ReadDir(root)
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		chats, _ := os.ReadDir(filepath.Join(root, p.Name()))
		for _, c := range chats {
			path := filepath.Join(root, p.Name(), c.Name(), "store.db")
			stamp, ok := fileStamp(path)
			if !ok {
				continue
			}
			out = append(out, connCache.get("cursor-cli:"+path, stamp, 0, func() []*SessionRecord {
				if r := parseCursorCLI(path, c.Name()); r != nil {
					return []*SessionRecord{r}
				}
				return nil
			})...)
		}
	}
	return newestFirst(out)
}

func parseCursorCLI(path, id string) *SessionRecord {
	db, err := openReadOnly(path)
	if err != nil {
		return nil
	}
	defer db.Close()
	var hexMeta string
	if db.QueryRow(`SELECT value FROM meta WHERE key = '0'`).Scan(&hexMeta) != nil {
		return nil
	}
	rawMeta, err := hex.DecodeString(strings.TrimSpace(hexMeta))
	if err != nil {
		rawMeta = []byte(hexMeta)
	}
	var meta struct {
		AgentID    string  `json:"agentId"`
		Root       string  `json:"latestRootBlobId"`
		Name       string  `json:"name"`
		CreatedAt  float64 `json:"createdAt"`
		LastModel  string  `json:"lastUsedModel"`
		WorkingDir string  `json:"workspacePath"`
	}
	_ = json.Unmarshal(rawMeta, &meta)
	blobs := map[string][]byte{}
	rows, err := db.Query(`SELECT id, data FROM blobs`)
	if err != nil {
		return nil
	}
	var order []string
	for rows.Next() {
		var bid string
		var data []byte
		if rows.Scan(&bid, &data) == nil {
			blobs[bid] = data
			order = append(order, bid)
		}
	}
	rows.Close()
	var msgs [][]byte
	seen := map[string]bool{}
	var walk func(id string, depth int)
	walk = func(id string, depth int) {
		data, ok := blobs[id]
		if !ok || len(data) == 0 || seen[id] || depth > 10000 {
			return
		}
		seen[id] = true
		if data[0] == '{' {
			msgs = append(msgs, data)
			return
		}
		off := 0
		for off+34 <= len(data) && data[off] == 0x0A && data[off+1] == 0x20 {
			walk(hex.EncodeToString(data[off+2:off+34]), depth+1)
			off += 34
		}
		if i := strings.IndexByte(string(data[off:]), '{'); i >= 0 {
			msgs = append(msgs, data[off+i:])
		}
	}
	if meta.Root != "" {
		walk(meta.Root, 0)
	}
	if len(msgs) == 0 { // no tree: every JSON blob in storage order
		for _, bid := range order {
			if d := blobs[bid]; len(d) > 0 && d[0] == '{' {
				msgs = append(msgs, d)
			}
		}
	}
	rec := &SessionRecord{
		SessionID: cmp.Or(meta.AgentID, id),
		Title:     meta.Name,
		Harness:   "cursor-cli",
		Model:     meta.LastModel,
		CWD:       meta.WorkingDir,
		FilePath:  path,
		UpdatedAt: msTime(meta.CreatedAt),
	}
	if fi, err := os.Stat(path); err == nil && fi.ModTime().After(rec.UpdatedAt) {
		rec.UpdatedAt = fi.ModTime().UTC()
	}
	if rec.Title == "New Agent" {
		rec.Title = ""
	}
	for _, m := range msgs {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m, &msg) != nil {
			continue
		}
		role := normalizeRole(msg.Role)
		if role == "" {
			continue
		}
		var s string
		if json.Unmarshal(msg.Content, &s) == nil {
			if role == "user" {
				s = cmp.Or(tagBody(s, "user_query"), s)
				if strings.HasPrefix(strings.TrimSpace(s), "<") && tagBody(s, "user_query") == "" && strings.Contains(s, "<user_info>") {
					continue // the injected system context
				}
			}
			if tr, ok := convTurn(role, s, time.Time{}); ok {
				rec.Turns = append(rec.Turns, tr)
			}
			continue
		}
		var parts []map[string]any
		if json.Unmarshal(msg.Content, &parts) != nil {
			continue
		}
		for _, p := range parts {
			switch stringVal(p["type"]) {
			case "text":
				t := stringVal(p["text"])
				if role == "user" {
					t = cmp.Or(tagBody(t, "user_query"), t)
				}
				if tr, ok := convTurn(role, t, time.Time{}); ok {
					rec.Turns = append(rec.Turns, tr)
				}
			case "tool-call", "tool_use":
				name := stringVal(p["toolName"])
				args, _ := json.Marshal(p["args"])
				if tr, ok := toolTurn(name, name+" "+clipRunes(string(args), 300), false, time.Time{}); ok {
					rec.Turns = append(rec.Turns, tr)
				}
			case "tool-result", "tool_result":
				res, _ := json.Marshal(p["result"])
				if tr, ok := toolTurn(stringVal(p["toolName"]), string(res), p["isError"] == true, time.Time{}); ok {
					rec.Turns = append(rec.Turns, tr)
				}
			}
		}
	}
	return finishRecord(rec)
}

func tagBody(s, tag string) string {
	open, closeTag := "<"+tag+">", "</"+tag+">"
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	if j := strings.Index(rest, closeTag); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest)
}

// cursorCLIFolderHash is how the Cursor CLI names a folder's chat directory (tests).
func cursorCLIFolderHash(folder string) string {
	sum := md5.Sum([]byte(folder))
	return hex.EncodeToString(sum[:])
}

// ---- Antigravity ----

var agyModelRe = regexp.MustCompile(` from .*? to (.+?)(?:\. |\.?$)`)

func antigravitySessions(home string) []*SessionRecord {
	var out []*SessionRecord
	for _, base := range []string{"antigravity", "antigravity-cli"} {
		brain := filepath.Join(home, ".gemini", base, "brain")
		ents, err := os.ReadDir(brain)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(brain, e.Name())
			stamp := dirStamp(dir)
			if stamp == "" {
				continue
			}
			id, layout := e.Name(), base
			out = append(out, connCache.get("agy:"+dir, stamp, 0, func() []*SessionRecord {
				if r := parseAntigravity(dir, id, layout); r != nil {
					return []*SessionRecord{r}
				}
				return nil
			})...)
		}
	}
	return newestFirst(out)
}

var agyArtifacts = []string{"task.md", "implementation_plan.md", "walkthrough.md"}

// dirStamp is the size and time of a conversation's transcript and artifacts ("" when it has none).
func dirStamp(dir string) string {
	var b strings.Builder
	for _, p := range append([]string{filepath.Join(".system_generated", "logs", "transcript.jsonl")}, agyArtifacts...) {
		if fi, err := os.Stat(filepath.Join(dir, p)); err == nil {
			fmt.Fprintf(&b, "%s:%d/%d;", p, fi.Size(), fi.ModTime().UnixNano())
		}
	}
	return b.String()
}

func parseAntigravity(dir, id, layout string) *SessionRecord {
	rec := &SessionRecord{SessionID: id, Harness: "antigravity", FilePath: dir}
	if layout == "antigravity-cli" {
		rec.SessionID = "cli-" + id // the CLI and the IDE keep separate brains; ids must not meet
	}
	type step struct {
		Index     int              `json:"step_index"`
		Source    string           `json:"source"`
		Type      string           `json:"type"`
		Status    string           `json:"status"`
		CreatedAt string           `json:"created_at"`
		Content   string           `json:"content"`
		ToolCalls []map[string]any `json:"tool_calls"`
	}
	var steps []step
	if f, err := os.Open(filepath.Join(dir, ".system_generated", "logs", "transcript.jsonl")); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), maxSessionFileBytes)
		for sc.Scan() {
			var s step
			if json.Unmarshal(sc.Bytes(), &s) == nil {
				steps = append(steps, s)
			}
		}
		f.Close()
	}
	slices.SortStableFunc(steps, func(a, b step) int { return cmp.Compare(a.Index, b.Index) })
	for _, s := range steps {
		at := parseRFC3339(s.CreatedAt)
		switch {
		case s.Type == "USER_INPUT":
			if rec.Model == "" {
				if m := agyModelRe.FindStringSubmatch(tagBody(s.Content, "USER_SETTINGS_CHANGE")); m != nil && !strings.EqualFold(m[1], "none") {
					rec.Model = strings.TrimSpace(m[1])
				}
			}
			if tr, ok := convTurn("user", cmp.Or(tagBody(s.Content, "USER_REQUEST"), s.Content), at); ok {
				tr.Line = s.Index + 1
				rec.Turns = append(rec.Turns, tr)
			}
		case s.Type == "PLANNER_RESPONSE":
			if tr, ok := convTurn("assistant", s.Content, at); ok {
				tr.Line = s.Index + 1
				rec.Turns = append(rec.Turns, tr)
			}
		case s.Source == "MODEL":
			text := s.Content
			for _, tc := range s.ToolCalls {
				if rec.CWD == "" {
					if args, ok := tc["args"].(map[string]any); ok {
						rec.CWD = cmp.Or(stringVal(args["Cwd"]), stringVal(args["cwd"]))
					}
				}
			}
			if strings.TrimSpace(text) == "" && len(s.ToolCalls) > 0 {
				raw, _ := json.Marshal(s.ToolCalls)
				text = string(raw)
			}
			if tr, ok := toolTurn(strings.ToLower(s.Type), s.Type+" "+text, s.Status == "ERROR" || s.Status == "FAILED", at); ok {
				tr.Line = s.Index + 1
				rec.Turns = append(rec.Turns, tr)
			}
		}
	}
	// The brain's artifacts: the plan, the task list and the walkthrough the agent wrote.
	next := len(steps) + 1000
	for _, name := range agyArtifacts {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil || len(raw) == 0 {
			continue
		}
		fi, _ := os.Stat(path)
		at := fi.ModTime().UTC()
		var meta struct {
			Summary   string `json:"summary"`
			UpdatedAt string `json:"updatedAt"`
		}
		if m, err := os.ReadFile(path + ".metadata.json"); err == nil && json.Unmarshal(m, &meta) == nil {
			at = cmp.Or(parseRFC3339(meta.UpdatedAt), at)
		}
		text := "[" + name + "] " + string(raw)
		if tr, ok := convTurn("assistant", text, at); ok {
			next++
			tr.Line = next
			rec.Turns = append(rec.Turns, tr)
		}
		if rec.Title == "" && name == "task.md" {
			rec.Title = firstLineTitle(string(raw))
		}
	}
	if len(rec.Turns) == 0 {
		return nil
	}
	return finishRecord(rec)
}

// ---- Gemini CLI ----

func geminiCLISessions(home string) []*SessionRecord {
	root := filepath.Join(home, ".gemini", "tmp")
	projects, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []*SessionRecord
	for _, p := range projects {
		chats := filepath.Join(root, p.Name(), "chats")
		ents, _ := os.ReadDir(chats)
		for _, e := range ents {
			name := e.Name()
			if e.IsDir() || !strings.HasPrefix(name, "session-") || (!strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".jsonl")) {
				continue
			}
			fi, err := e.Info()
			if err != nil || fi.Size() == 0 || fi.Size() > 64<<20 {
				continue
			}
			path := filepath.Join(chats, name)
			if rec := cachedParseWith(fileEntry{path: path, mod: fi.ModTime(), size: fi.Size()}, parseGeminiCLI); rec != nil {
				out = append(out, rec)
			}
		}
	}
	return newestFirst(out)
}

type geminiMessage struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Content   any    `json:"content"`
	Model     string `json:"model"`
	ToolCalls []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Result any    `json:"result"`
		Args   any    `json:"args"`
	} `json:"toolCalls"`
}

type geminiSession struct {
	SessionID   string          `json:"sessionId"`
	ProjectHash string          `json:"projectHash"`
	StartTime   string          `json:"startTime"`
	LastUpdated string          `json:"lastUpdated"`
	Messages    []geminiMessage `json:"messages"`
}

func parseGeminiCLI(path string, mod time.Time) *SessionRecord {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var s geminiSession
	if strings.HasSuffix(path, ".json") {
		if json.Unmarshal(raw, &s) != nil {
			return nil
		}
	} else {
		// JSONL: a header, messages upserted by id, and {"$set": {...}} patches ({"messages": [...]} replaces).
		idx := map[string]int{}
		upsert := func(m geminiMessage) {
			if i, ok := idx[m.ID]; ok && m.ID != "" {
				s.Messages[i] = m
				return
			}
			if m.ID != "" {
				idx[m.ID] = len(s.Messages)
			}
			s.Messages = append(s.Messages, m)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var obj map[string]json.RawMessage
			if json.Unmarshal([]byte(line), &obj) != nil {
				continue
			}
			if set, ok := obj["$set"]; ok {
				var patch geminiSession
				if json.Unmarshal(set, &patch) == nil {
					var hasMsgs map[string]json.RawMessage
					_ = json.Unmarshal(set, &hasMsgs)
					if _, ok := hasMsgs["messages"]; ok {
						s.Messages, idx = nil, map[string]int{}
						for _, m := range patch.Messages {
							upsert(m)
						}
					}
					s.LastUpdated = cmp.Or(patch.LastUpdated, s.LastUpdated)
				}
				continue
			}
			if _, ok := obj["sessionId"]; ok {
				var h geminiSession
				if json.Unmarshal([]byte(line), &h) == nil {
					s.SessionID, s.ProjectHash = cmp.Or(h.SessionID, s.SessionID), cmp.Or(h.ProjectHash, s.ProjectHash)
					s.StartTime, s.LastUpdated = cmp.Or(h.StartTime, s.StartTime), cmp.Or(h.LastUpdated, s.LastUpdated)
					for _, m := range h.Messages {
						upsert(m)
					}
				}
				continue
			}
			var m geminiMessage
			if json.Unmarshal([]byte(line), &m) == nil && m.Type != "" {
				upsert(m)
			}
		}
	}
	rec := &SessionRecord{
		SessionID: cmp.Or(s.SessionID, strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))),
		Harness:   "gemini-cli",
		FilePath:  path,
		UpdatedAt: cmp.Or(parseRFC3339(s.LastUpdated), mod.UTC()),
	}
	for _, m := range s.Messages {
		at := parseRFC3339(m.Timestamp)
		text := geminiText(m.Content)
		role := ""
		switch m.Type {
		case "user":
			role = "user"
		case "gemini", "model", "assistant":
			role = "assistant"
			if rec.Model == "" {
				rec.Model = m.Model
			}
		case "error":
			if tr, ok := toolTurn("error", text, true, at); ok {
				rec.Turns = append(rec.Turns, tr)
			}
		}
		if role != "" {
			if tr, ok := convTurn(role, text, at); ok {
				rec.Turns = append(rec.Turns, tr)
			}
		}
		for _, tc := range m.ToolCalls {
			res, _ := json.Marshal(tc.Result)
			args, _ := json.Marshal(tc.Args)
			if rec.CWD == "" {
				if a, ok := tc.Args.(map[string]any); ok {
					if d := cmp.Or(stringVal(a["dir_path"]), stringVal(a["directory"])); filepath.IsAbs(d) {
						rec.CWD = d
					}
				}
			}
			if tr, ok := toolTurn(tc.Name, tc.Name+" "+clipRunes(string(args), 300)+"\n"+string(res), tc.Status == "error", at); ok {
				rec.Turns = append(rec.Turns, tr)
			}
		}
	}
	if len(rec.Turns) == 0 {
		return nil
	}
	return finishRecord(rec)
}

func geminiText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			if m, ok := p.(map[string]any); ok {
				if t := stringVal(m["text"]); t != "" {
					parts = append(parts, t)
				}
			} else if s, ok := p.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
