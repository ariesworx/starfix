package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"time"
)

// runVectors loads n random vectors and compares Dolt's vector index with an
// exact brute-force scan in Go: query latency and recall@k.
func runVectors(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("vectors", flag.ExitOnError)
	n := fs.Int("n", 50000, "vectors")
	dim := fs.Int("dim", 768, "dimensions")
	clusters := fs.Int("clusters", 200, "Gaussian clusters (0: uniform random, which has no real neighbors)")
	nq := fs.Int("queries", 50, "queries")
	k := fs.Int("k", 10, "neighbors")
	scanQ := fs.Int("scanq", 5, "queries to time as an unindexed SQL scan before building the index")
	binaryIn := fs.Bool("binary", false, "send vectors as little-endian float32 bytes instead of STRING_TO_VECTOR text")
	fs.Parse(args)

	r := rand.New(rand.NewPCG(1, 2))
	gen := newGen(r, *dim, *clusters)
	data := make([][]float32, *n)
	for i := range data {
		data[i] = gen.next()
	}
	queries := make([][]float32, *nq)
	for i := range queries {
		queries[i] = gen.next()
	}

	db, err := open(ctx, true, 4)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := execAll(ctx, db, fmt.Sprintf("CREATE TABLE emb (id INT PRIMARY KEY, v VECTOR(%d) NOT NULL)", *dim)); err != nil {
		return err
	}

	t0 := time.Now()
	const batch = 200
	for s := 0; s < *n; s += batch {
		e := min(s+batch, *n)
		var b strings.Builder
		b.WriteString("INSERT INTO emb (id, v) VALUES ")
		params := make([]any, 0, 2*(e-s))
		for i := s; i < e; i++ {
			if i > s {
				b.WriteByte(',')
			}
			if *binaryIn {
				b.WriteString("(?, ?)")
				params = append(params, i, vecBytes(data[i]))
			} else {
				b.WriteString("(?, STRING_TO_VECTOR(?))")
				params = append(params, i, vecText(data[i]))
			}
		}
		if _, err := db.ExecContext(ctx, b.String(), params...); err != nil {
			return fmt.Errorf("insert at %d: %w", s, err)
		}
	}
	load := time.Since(t0)
	fmt.Printf("vectors n=%d dim=%d clusters=%d binary=%v load=%.1fs (%.0f rows/s)\n", *n, *dim, *clusters, *binaryIn, load.Seconds(), float64(*n)/load.Seconds())

	exact := make([][]int, *nq)
	var goLat latencies
	for qi, q := range queries {
		t := time.Now()
		exact[qi] = bruteForce(data, q, *k)
		goLat = append(goLat, time.Since(t))
	}
	fmt.Printf("go brute force (L2 on unit vectors = cosine order): p50=%sms p99=%sms\n", ms(goLat.pct(.5)), ms(goLat.pct(.99)))

	// The index is only chosen when the query vector is a bare string, not
	// STRING_TO_VECTOR(...) (checked with EXPLAIN PLAN in Dolt 2.4.2).
	const qsql = "SELECT id FROM emb ORDER BY VEC_DISTANCE(v, ?) LIMIT ?"
	if *scanQ > 0 {
		lat, rec, err := sqlQueries(ctx, db, qsql, queries[:*scanQ], exact, *k)
		if err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		fmt.Printf("dolt SQL scan, no index (%d queries): p50=%sms p99=%sms recall@%d=%.3f\n", *scanQ, ms(lat.pct(.5)), ms(lat.pct(.99)), *k, rec)
	}

	t0 = time.Now()
	if err := execAll(ctx, db, "ALTER TABLE emb ADD VECTOR INDEX vidx (v)"); err != nil {
		return err
	}
	fmt.Printf("vector index build: %.1fs\n", time.Since(t0).Seconds())
	var plan strings.Builder
	rows, err := db.QueryContext(ctx, fmt.Sprintf("EXPLAIN PLAN SELECT id FROM emb ORDER BY VEC_DISTANCE(v, '%s') LIMIT %d", vecText(queries[0]), *k))
	if err != nil {
		fmt.Println("explain:", err)
	} else {
		for rows.Next() {
			var s string
			rows.Scan(&s)
			plan.WriteString(strings.TrimSpace(s) + " | ")
		}
		rows.Close()
	}
	p := plan.String()
	fmt.Printf("plan (literal query vector) uses the vector index: %v\n", strings.Contains(p, "IndexedTableAccess"))
	lat, rec, err := sqlQueries(ctx, db, qsql, queries, exact, *k)
	if err != nil {
		return err
	}
	fmt.Printf("dolt, placeholder query vector (%d queries): p50=%sms p99=%sms recall@%d=%.3f\n", *nq, ms(lat.pct(.5)), ms(lat.pct(.99)), *k, rec)
	lat, rec, err = sqlQueries(ctx, db, "inline", queries, exact, *k)
	if err != nil {
		return err
	}
	fmt.Printf("dolt, literal query vector (%d queries): p50=%sms p99=%sms recall@%d=%.3f\n", *nq, ms(lat.pct(.5)), ms(lat.pct(.99)), *k, rec)
	return nil
}

func sqlQueries(ctx context.Context, db *sql.DB, q string, queries [][]float32, exact [][]int, k int) (latencies, float64, error) {
	var lat latencies
	var hits, total int
	for qi, v := range queries {
		t := time.Now()
		var rows *sql.Rows
		var err error
		if q == "inline" {
			rows, err = db.QueryContext(ctx, fmt.Sprintf("SELECT id FROM emb ORDER BY VEC_DISTANCE(v, '%s') LIMIT %d", vecText(v), k))
		} else {
			rows, err = db.QueryContext(ctx, q, vecText(v), k)
		}
		if err != nil {
			return nil, 0, err
		}
		got := map[int]bool{}
		for rows.Next() {
			var id int
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, 0, err
			}
			got[id] = true
		}
		rows.Close()
		lat = append(lat, time.Since(t))
		for _, id := range exact[qi] {
			if got[id] {
				hits++
			}
		}
		total += len(exact[qi])
	}
	return lat, float64(hits) / float64(total), nil
}

type gen struct {
	r       *rand.Rand
	dim     int
	centers [][]float32
}

func newGen(r *rand.Rand, dim, clusters int) *gen {
	g := &gen{r: r, dim: dim}
	for range clusters {
		g.centers = append(g.centers, g.normal(1))
	}
	return g
}

func (g *gen) normal(scale float64) []float32 {
	v := make([]float32, g.dim)
	for i := range v {
		v[i] = float32(g.r.NormFloat64() * scale)
	}
	return v
}

// next returns a unit vector: uniform on the sphere, or a cluster center
// plus noise.
func (g *gen) next() []float32 {
	v := g.normal(1)
	if len(g.centers) > 0 {
		c := g.centers[g.r.IntN(len(g.centers))]
		for i := range v {
			v[i] = c[i] + 0.6*v[i]
		}
	}
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	inv := float32(1 / math.Sqrt(s))
	for i := range v {
		v[i] *= inv
	}
	return v
}

func vecText(v []float32) string {
	var b strings.Builder
	b.Grow(len(v) * 10)
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', 7, 32))
	}
	b.WriteByte(']')
	return b.String()
}

func vecBytes(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

func bruteForce(data [][]float32, q []float32, k int) []int {
	type hit struct {
		id int
		d  float32
	}
	top := make([]hit, 0, k+1)
	for id, v := range data {
		var d float32
		for i := range v {
			x := v[i] - q[i]
			d += x * x
		}
		if len(top) < k || d < top[len(top)-1].d {
			top = append(top, hit{id, d})
			sort.Slice(top, func(i, j int) bool { return top[i].d < top[j].d })
			if len(top) > k {
				top = top[:k]
			}
		}
	}
	ids := make([]int, len(top))
	for i, h := range top {
		ids[i] = h.id
	}
	return ids
}
