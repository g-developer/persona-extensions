package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"
)

// Embedders for the hub's central vector index (vectors.go).
//
//   - hashEmbedder (semantic: "hash"): built in, no model and no network. Feature hashing of words, CJK bigrams
//     and character trigrams into 384 signed dimensions. It is lexical, not semantic: it adds tolerance to
//     spelling, inflection and partial CJK matches, never synonyms or another language.
//   - httpEmbedder (semantic: "endpoint"): an OpenAI-compatible POST <url>/embeddings on the hub's network,
//     e.g. Ollama (http://127.0.0.1:11434/v1) with a multilingual model such as bge-m3, which matches a Chinese
//     question to an English conversation. Only the hub calls it, with conversation excerpts of the sessions it
//     indexes and the queries; nodes never do.

type embedder interface {
	// ID names the vector space: vectors of different IDs are never compared.
	ID() string
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// band is the cosine range from "unrelated" to "clearly related" for this embedder, used to turn a
	// similarity into a confidence in [0, 1].
	band() (lo, hi float64)
}

const (
	semanticOff      = "off"
	semanticHash     = "hash"
	semanticEndpoint = "endpoint"
)

func validSemantic(s string) bool {
	return s == semanticOff || s == semanticHash || s == semanticEndpoint
}

// newEmbedder is the embedder a setting names, nil when off or incomplete.
func newEmbedder(mode, url, model string) embedder {
	switch mode {
	case semanticHash:
		return hashEmbedder{dim: hashDim}
	case semanticEndpoint:
		url, model = strings.TrimRight(strings.TrimSpace(url), "/"), strings.TrimSpace(model)
		if url == "" || model == "" {
			return nil
		}
		return &httpEmbedder{url: url, model: model, key: os.Getenv("PERSONA_SEARCH_AS_CODE_EMBED_KEY"),
			client: &http.Client{Timeout: 60 * time.Second}}
	}
	return nil
}

const hashDim = 384

type hashEmbedder struct{ dim int }

func (h hashEmbedder) ID() string               { return fmt.Sprintf("hash-v1-%d", h.dim) }
func (h hashEmbedder) band() (float64, float64) { return 0.12, 0.55 }

func (h hashEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = h.one(t)
	}
	return out, nil
}

func (h hashEmbedder) one(text string) []float32 {
	type acc struct {
		w float64
		n int
	}
	counts := map[string]acc{}
	for _, f := range hashFeatures(strings.ToLower(text)) {
		a := counts[f.key]
		a.w, a.n = f.w, a.n+1
		counts[f.key] = a
	}
	v := make([]float32, h.dim)
	for k, c := range counts {
		hs := fnv.New64a()
		_, _ = hs.Write([]byte(k))
		x := hs.Sum64()
		sign := float32(1)
		if x>>63 == 1 {
			sign = -1
		}
		v[x%uint64(h.dim)] += sign * float32(c.w*(1+math.Log(float64(c.n))))
	}
	normalize(v)
	return v
}

type feature struct {
	key string
	w   float64
}

// hashFeatures are the words of text (weight 1), its CJK bigrams (1) and the character trigrams of its longer
// words (0.4, marked so they never collide with a word).
func hashFeatures(low string) []feature {
	var out []feature
	var word []rune
	var cjk []rune
	flushWord := func() {
		if len(word) > 1 && len(word) <= 40 {
			out = append(out, feature{"w:" + string(word), 1})
			if len(word) >= 4 {
				p := append([]rune{'^'}, append(word, '$')...)
				for i := 0; i+3 <= len(p); i++ {
					out = append(out, feature{"t:" + string(p[i:i+3]), 0.4})
				}
			}
		}
		word = word[:0]
	}
	flushCJK := func() {
		if len(cjk) == 1 {
			out = append(out, feature{"c:" + string(cjk), 0.5})
		}
		for i := 0; i+1 < len(cjk); i++ {
			out = append(out, feature{"c:" + string(cjk[i:i+2]), 1})
		}
		cjk = cjk[:0]
	}
	for _, r := range low {
		switch {
		case isCJK(r):
			flushWord()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			word = append(word, r)
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
	return out
}

func normalize(v []float32) {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	if n == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(n))
	for i := range v {
		v[i] *= inv
	}
}

type httpEmbedder struct {
	url, model, key string
	client          *http.Client
}

func (e *httpEmbedder) ID() string               { return "endpoint|" + e.url + "|" + e.model }
func (e *httpEmbedder) band() (float64, float64) { return 0.35, 0.75 }

func (e *httpEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": e.model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.key != "" {
		req.Header.Set("Authorization", "Bearer "+e.key)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("embedding endpoint: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("embedding endpoint: %w", err)
	}
	if len(out.Data) != len(texts) {
		return nil, errors.New("embedding endpoint: wrong number of vectors")
	}
	vecs := make([][]float32, len(texts))
	for i, d := range out.Data {
		j := d.Index
		if j < 0 || j >= len(texts) || vecs[j] != nil {
			j = i
		}
		normalize(d.Embedding)
		vecs[j] = d.Embedding
	}
	return vecs, nil
}
