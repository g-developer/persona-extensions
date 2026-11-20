package main

import (
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Lexical relevance (CASS-style keyword search, without an on-disk inverted index).
//
// The query is tokenized; documents are not: a term is counted in a turn's lower-cased text directly (a short
// ASCII term only at word boundaries, so "go" does not match "google"). A run of CJK characters in the query
// becomes overlapping bigrams, the usual way to search CJK text without a dictionary, so 数据库超时 also finds
// 数据库连接超时. A quoted "phrase" is one term; a -term excludes every session holding it.
//
// Scores are BM25 (k1 1.2, b 0.75) over the turns in scope, with the document frequencies of this corpus, and
// normalized by the score a turn holding every term (saturated) would reach: a session's lexical relevance is in
// [0, 1] whatever the corpus, so the scores of different computers can be merged as they are (remote.go) and
// combined with semantic similarity (vectors.go).

const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// queryTerms is a tokenized query.
type queryTerms struct {
	Terms    []string // positive terms, lower-cased, deduplicated
	Negative []string
	Raw      []string // the words as written (for display and the legacy any-term title match)
}

func (q queryTerms) empty() bool { return len(q.Terms) == 0 }

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}

// parseQueryTerms tokenizes a query: inline key:value tokens are skipped (the compiler takes them), quoted
// phrases kept whole, -words negated, CJK runs split into bigrams, other words split at punctuation.
func parseQueryTerms(q string) queryTerms {
	var out queryTerms
	add := func(list *[]string, t string) {
		t = strings.TrimSpace(t)
		if t == "" {
			return
		}
		for _, x := range *list {
			if x == t {
				return
			}
		}
		*list = append(*list, t)
	}
	q = strings.ToLower(strings.TrimSpace(q))
	for q != "" {
		q = strings.TrimLeft(q, " \t\n")
		if q == "" {
			break
		}
		neg := false
		if q[0] == '-' && len(q) > 1 && q[1] != ' ' && q[1] != '-' {
			neg, q = true, q[1:]
		}
		var word string
		if q[0] == '"' {
			if end := strings.IndexByte(q[1:], '"'); end >= 0 {
				word, q = q[1:end+1], q[end+2:]
				if neg {
					add(&out.Negative, word)
				} else {
					add(&out.Terms, word)
					add(&out.Raw, word)
				}
				continue
			}
			q = q[1:]
		}
		if i := strings.IndexAny(q, " \t\n"); i >= 0 {
			word, q = q[:i], q[i:]
		} else {
			word, q = q, ""
		}
		if k, v, ok := strings.Cut(word, ":"); ok && k != "" && v != "" && !strings.Contains(k, "/") && isInlineKey(k) {
			continue
		}
		word = strings.Trim(word, `"'(),.;!?，。；！？、（）`)
		if word == "" {
			continue
		}
		if neg {
			add(&out.Negative, word)
			continue
		}
		add(&out.Raw, word)
		for _, t := range splitTerm(word) {
			add(&out.Terms, t)
		}
	}
	return out
}

func isInlineKey(k string) bool {
	switch k {
	case "scope", "project", "proj", "repo", "harness", "agent", "model", "since", "after", "machine", "host",
		"computer", "regex", "re", "group", "group_by", "correlate", "join", "file", "glob", "path", "without",
		"exclude", "mode":
		return true
	}
	return false
}

// splitTerm splits a query word: CJK runs into bigrams (a single CJK character stays itself), the rest into
// alphanumeric pieces; a word with inner punctuation (nodes.call, foo_bar.go) is kept whole as well.
func splitTerm(w string) []string {
	var out []string
	var cjk []rune
	var other strings.Builder
	flushCJK := func() {
		switch len(cjk) {
		case 0:
		case 1:
			out = append(out, string(cjk))
		default:
			for i := 0; i+1 < len(cjk); i++ {
				out = append(out, string(cjk[i:i+2]))
			}
		}
		cjk = cjk[:0]
	}
	flushOther := func() {
		if s := other.String(); s != "" {
			parts := strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
			if len(parts) > 1 && utf8.RuneCountInString(s) <= 64 {
				out = append(out, strings.Trim(s, "-_./:"))
			}
			out = append(out, parts...)
		}
		other.Reset()
	}
	for _, r := range w {
		if isCJK(r) {
			flushOther()
			cjk = append(cjk, r)
			continue
		}
		flushCJK()
		other.WriteRune(r)
	}
	flushCJK()
	flushOther()
	return out
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// termFreq counts term in low (already lower-cased). A short ASCII alphanumeric term counts only where it is not
// part of a longer word.
func termFreq(low, term string) int {
	if term == "" {
		return 0
	}
	bounded := len(term) <= 3 && isWordByte(term[0]) && isWordByte(term[len(term)-1])
	n, from := 0, 0
	for {
		i := strings.Index(low[from:], term)
		if i < 0 {
			return n
		}
		i += from
		end := i + len(term)
		if !bounded || ((i == 0 || !isWordByte(low[i-1])) && (end == len(low) || !isWordByte(low[end]))) {
			n++
		}
		from = i + 1
		if n >= 50 {
			return n
		}
	}
}

// lexDoc is one scored turn of the corpus a query is run on.
type lexDoc struct {
	tf  []int // per query term
	len int   // in bytes / 6, a cheap stand-in for a token count
	neg bool
}

// lexCorpus holds the term statistics of one query over the turns in scope.
type lexCorpus struct {
	q      queryTerms
	df     []int
	n      int
	avgLen float64
	idf    []float64
	ideal  float64
}

func newLexCorpus(q queryTerms) *lexCorpus {
	return &lexCorpus{q: q, df: make([]int, len(q.Terms))}
}

// observe measures one turn and counts it into the statistics.
func (c *lexCorpus) observe(text string) lexDoc {
	low := strings.ToLower(text)
	d := lexDoc{tf: make([]int, len(c.q.Terms)), len: max(len(low)/6, 1)}
	for _, t := range c.q.Negative {
		if termFreq(low, t) > 0 {
			d.neg = true
			break
		}
	}
	for i, t := range c.q.Terms {
		if f := termFreq(low, t); f > 0 {
			d.tf[i] = f
			c.df[i]++
		}
	}
	c.n++
	c.avgLen += float64(d.len)
	return d
}

// finish computes the idf of every term once all turns are observed.
func (c *lexCorpus) finish() {
	if c.n > 0 {
		c.avgLen /= float64(c.n)
	}
	c.idf = make([]float64, len(c.q.Terms))
	c.ideal = 0
	for i, df := range c.df {
		c.idf[i] = math.Log(1 + (float64(c.n)-float64(df)+0.5)/(float64(df)+0.5))
		c.ideal += c.idf[i] * (bm25K1 + 1)
	}
}

// score is a turn's BM25 normalized to [0, 1] and how many distinct terms it holds.
func (c *lexCorpus) score(d lexDoc) (float64, int) {
	if d.neg || c.ideal == 0 {
		return 0, 0
	}
	s, hits := 0.0, 0
	norm := bm25K1 * (1 - bm25B + bm25B*float64(d.len)/max(c.avgLen, 1))
	for i, f := range d.tf {
		if f == 0 {
			continue
		}
		hits++
		tf := float64(f)
		s += c.idf[i] * tf * (bm25K1 + 1) / (tf + norm)
	}
	return s / c.ideal, hits
}

// titleScore is the share of the query's terms (idf-weighted) a title holds.
func (c *lexCorpus) titleScore(title string) float64 {
	if c.ideal == 0 {
		return 0
	}
	low := strings.ToLower(title)
	s := 0.0
	for i, t := range c.q.Terms {
		if termFreq(low, t) > 0 {
			s += c.idf[i] * (bm25K1 + 1)
		}
	}
	return s / c.ideal
}
