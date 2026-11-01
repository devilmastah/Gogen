package codeindex

import (
	"encoding/binary"
	"hash/fnv"
	"math"
	"math/rand"
	"sort"
	"strings"
)

// approxMinChunks is when search may use the in-memory lookup. Below this
// count the scan compares every stored vector. 20,000 is the cutoff in the
// plan. It is not a measured latency. Tests lower it and restore it, and
// those tests do not run in parallel.
var approxMinChunks = 20000

const (
	approxM        = 16
	approxEfBuild  = 64
	approxEfSearch = 64
	approxMaxLevel = 16
)

// approxLevelMult is 1/ln(M), the level-generation scale from the HNSW paper.
var approxLevelMult = 1 / math.Log(float64(approxM))

// approxGen is the identity of the vectors a lookup was built from. A file
// change publishes a new vector sequence, and a different gen rebuilds the
// lookup. A refresh that fails or changes nothing leaves the gen in place.
type approxGen struct {
	chunks int
	dim    int
	sum    uint64
}

// hnsw is a hierarchical navigable small world graph over one snapshot of
// the stored vectors. It is not written to disk.
type hnsw struct {
	vecs  [][]float32
	links [][][]int
	entry int
	gen   approxGen
	rng   *rand.Rand
}

type nn struct {
	i   int
	sim float64
}

// prepareApprox builds the lookup from st when the chunk count is at the
// cutoff. The same vector sequence keeps the lookup already on ix. A
// different sequence replaces it. Open calls this before the index is
// shared. Search and SetWorkingDir call it while holding mu.
//
// A changed file is re-embedded by the same refresh a search and the watch
// use. That refresh publishes vectors.bin, the next load reads it, and this
// rebuilds the graph from those vectors. The old vector is not searched.
func (ix *Index) prepareApprox(st stored) {
	if len(st.Chunks) < approxMinChunks || len(st.Vectors) != len(st.Chunks) {
		ix.approx = nil
		return
	}
	gen := vectorGen(st)
	if ix.approx != nil && ix.approx.gen == gen {
		return
	}
	ix.approx = buildHNSW(st.Vectors, gen)
}

func vectorGen(st stored) approxGen {
	h := fnv.New64a()
	var buf [4]byte
	for _, row := range st.Vectors {
		for _, v := range row {
			binary.LittleEndian.PutUint32(buf[:], math.Float32bits(v))
			_, _ = h.Write(buf[:])
		}
		_, _ = h.Write([]byte{0xff})
	}
	return approxGen{chunks: len(st.Chunks), dim: st.Meta.Dimension, sum: h.Sum64()}
}

// scoreChunks ranks chunks for one query. The whole index at the cutoff
// uses the in-memory lookup and keeps the closest KeepChunks, scored with
// the real cosine of the vectors just loaded. A path limit, and any index
// below the cutoff, compares every matching vector. The path limit stays
// exact because the lookup covers the whole index.
func (ix *Index) scoreChunks(st stored, q []float32, sub string) []scoredChunk {
	if sub == "" {
		ix.prepareApprox(st)
		if ix.approx != nil {
			if out := approxHits(ix.approx, st, q); len(out) > 0 {
				ix.approxUses++
				return out
			}
		}
	} else if len(st.Chunks) < approxMinChunks {
		ix.approx = nil
	}
	return exactScores(st, q, sub)
}

func approxHits(h *hnsw, st stored, q []float32) []scoredChunk {
	idx := h.search(q, KeepChunks)
	out := make([]scoredChunk, 0, len(idx))
	for _, i := range idx {
		if i < 0 || i >= len(st.Chunks) || i >= len(st.Vectors) {
			continue
		}
		d := st.Chunks[i]
		if !safeChunkPath(d.Path) {
			continue
		}
		out = append(out, scoredChunk{
			Chunk: Chunk{Path: d.Path, Start: d.Start, End: d.End, Hash: d.Hash},
			Score: cosine(q, st.Vectors[i]),
		})
	}
	return out
}

func exactScores(st stored, q []float32, sub string) []scoredChunk {
	var scored []scoredChunk
	for i, d := range st.Chunks {
		if !safeChunkPath(d.Path) {
			continue
		}
		if sub != "" && d.Path != sub && !strings.HasPrefix(d.Path, sub+"/") {
			continue
		}
		scored = append(scored, scoredChunk{
			Chunk: Chunk{Path: d.Path, Start: d.Start, End: d.End, Hash: d.Hash},
			Score: cosine(q, st.Vectors[i]),
		})
	}
	return scored
}

func buildHNSW(vecs [][]float32, gen approxGen) *hnsw {
	if len(vecs) == 0 {
		return nil
	}
	h := &hnsw{
		vecs:  vecs,
		links: make([][][]int, len(vecs)),
		gen:   gen,
		rng:   rand.New(rand.NewSource(1)),
	}
	for i := range vecs {
		h.insert(i)
	}
	h.rng = nil
	return h
}

func (h *hnsw) insert(i int) {
	lvl := 0
	if i != 0 {
		lvl = h.randomLevel()
	}
	h.links[i] = make([][]int, lvl+1)
	if i == 0 {
		h.entry = 0
		return
	}
	ep := h.entry
	top := len(h.links[ep]) - 1
	for lc := top; lc > lvl; lc-- {
		got := h.searchLayer(h.vecs[i], ep, 1, lc)
		if len(got) == 0 {
			break
		}
		ep = got[0].i
	}
	limit := lvl
	if top < limit {
		limit = top
	}
	for lc := limit; lc >= 0; lc-- {
		got := h.searchLayer(h.vecs[i], ep, approxEfBuild, lc)
		if len(got) == 0 {
			break
		}
		h.link(i, got, lc)
		ep = got[0].i
	}
	if lvl > top {
		h.entry = i
	}
}

func (h *hnsw) randomLevel() int {
	u := h.rng.Float64()
	if u < 1e-12 {
		u = 1e-12
	}
	lvl := int(math.Floor(-math.Log(u) * approxLevelMult))
	if lvl > approxMaxLevel {
		return approxMaxLevel
	}
	if lvl < 0 {
		return 0
	}
	return lvl
}

func (h *hnsw) link(i int, cands []nn, lc int) {
	maxN := approxMaxLinks(lc)
	added := 0
	for _, c := range cands {
		if c.i == i || c.i < 0 || c.i >= len(h.links) || lc >= len(h.links[c.i]) {
			continue
		}
		if !containsInt(h.links[i][lc], c.i) {
			h.links[i][lc] = append(h.links[i][lc], c.i)
		}
		if !containsInt(h.links[c.i][lc], i) {
			h.links[c.i][lc] = append(h.links[c.i][lc], i)
		}
		if len(h.links[c.i][lc]) > maxN {
			h.links[c.i][lc] = h.keepClosest(c.i, h.links[c.i][lc], maxN)
		}
		added++
		if added >= maxN {
			break
		}
	}
	if len(h.links[i][lc]) > maxN {
		h.links[i][lc] = h.keepClosest(i, h.links[i][lc], maxN)
	}
}

func approxMaxLinks(layer int) int {
	if layer == 0 {
		return approxM * 2
	}
	return approxM
}

func (h *hnsw) keepClosest(node int, ids []int, maxN int) []int {
	scored := make([]nn, 0, len(ids))
	seen := make(map[int]bool, len(ids))
	for _, id := range ids {
		if id == node || id < 0 || id >= len(h.vecs) || seen[id] {
			continue
		}
		seen[id] = true
		scored = append(scored, nn{i: id, sim: cosine(h.vecs[node], h.vecs[id])})
	}
	sort.Slice(scored, func(i, j int) bool { return betterNN(scored[i], scored[j]) })
	if len(scored) > maxN {
		scored = scored[:maxN]
	}
	out := make([]int, len(scored))
	for i, s := range scored {
		out[i] = s.i
	}
	return out
}

// search returns up to k nearest indexes, best cosine first.
func (h *hnsw) search(q []float32, k int) []int {
	if h == nil || len(h.vecs) == 0 || k <= 0 {
		return nil
	}
	if k > len(h.vecs) {
		k = len(h.vecs)
	}
	ep := h.entry
	top := len(h.links[ep]) - 1
	for lc := top; lc > 0; lc-- {
		got := h.searchLayer(q, ep, 1, lc)
		if len(got) == 0 {
			break
		}
		ep = got[0].i
	}
	ef := approxEfSearch
	if ef < k {
		ef = k
	}
	got := h.searchLayer(q, ep, ef, 0)
	out := make([]int, 0, k)
	seen := make(map[int]bool, len(got))
	for _, n := range got {
		if n.i < 0 || n.i >= len(h.vecs) || seen[n.i] {
			continue
		}
		seen[n.i] = true
		out = append(out, n.i)
		if len(out) == k {
			break
		}
	}
	return out
}

func (h *hnsw) searchLayer(q []float32, ep, ef, layer int) []nn {
	if ep < 0 || ep >= len(h.vecs) {
		return nil
	}
	if ef < 1 {
		ef = 1
	}
	visited := map[int]bool{ep: true}
	first := nn{i: ep, sim: cosine(q, h.vecs[ep])}
	cands := []nn{first}
	best := []nn{first}
	for len(cands) > 0 {
		cur := cands[0]
		cands = cands[1:]
		if len(best) >= ef && cur.sim < best[len(best)-1].sim {
			break
		}
		if cur.i < 0 || cur.i >= len(h.links) || layer >= len(h.links[cur.i]) {
			continue
		}
		for _, n := range h.links[cur.i][layer] {
			if n < 0 || n >= len(h.vecs) || visited[n] {
				continue
			}
			visited[n] = true
			s := cosine(q, h.vecs[n])
			if len(best) < ef || s > best[len(best)-1].sim {
				cands = insertNN(cands, nn{i: n, sim: s}, 0)
				best = insertNN(best, nn{i: n, sim: s}, ef)
			}
		}
	}
	return best
}

func insertNN(s []nn, x nn, max int) []nn {
	i := 0
	for i < len(s) && betterNN(s[i], x) {
		i++
	}
	s = append(s, nn{})
	copy(s[i+1:], s[i:])
	s[i] = x
	if max > 0 && len(s) > max {
		s = s[:max]
	}
	return s
}

func betterNN(a, b nn) bool {
	if a.sim != b.sim {
		return a.sim > b.sim
	}
	return a.i < b.i
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
