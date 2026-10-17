package api

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	_ "golang.org/x/image/webp"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var artKey = regexp.MustCompile(`^(w300|w500|w1280)/[A-Za-z0-9_-]+\.(jpg|jpeg|png|webp)$`)

type artwork struct {
	MIME  string `json:"mime"`
	Bytes []byte `json:"bytes"`
}

func validArtPath(p string) bool {
	return p == "" || strings.HasPrefix(p, "/internal/v1/artwork/") && artKey.MatchString(strings.TrimPrefix(p, "/internal/v1/artwork/"))
}
func validTitle(t Title) bool {
	if !t.Identity.Valid() || len(t.Title) > 300 || t.Runtime < 0 || t.MAL < 0 || !finite(t.Rating) || !validArtPath(t.Poster) || !validArtPath(t.Backdrop) {
		return false
	}
	if t.IMDb != "" && !imdbPattern.MatchString(t.IMDb) {
		return false
	}
	for _, v := range t.Seasons {
		if v.Number < 0 || v.Count < 0 {
			return false
		}
	}
	return true
}
func validTitles(t []Title) bool {
	if len(t) > 20000 {
		return false
	}
	for _, v := range t {
		if !validTitle(v) {
			return false
		}
	}
	return true
}
func validateSnapshot(v Snapshot) error {
	if v.Version != "1" || v.Revision < 0 || v.Exported.IsZero() || v.Exported.After(time.Now().Add(time.Minute)) || v.Records == nil || len(v.Records) > 20000 {
		return bad()
	}
	seen := map[string]bool{}
	for _, r := range v.Records {
		key := r.Kind + "/" + r.Key
		if r.Key == "" || len(r.Key) > 200 || seen[key] || r.Revision <= 0 || r.Revision > v.Revision || r.Updated.IsZero() || r.Updated.After(v.Exported) || r.Expires.IsZero() || r.Expires.Before(r.Updated) || (r.State != "fresh" && r.State != "stale") {
			return bad()
		}
		seen[key] = true
		switch r.Kind {
		case "title":
			var t Title
			if decode(r.Data, &t) != nil || !requiredKeys(r.Data, "type", "id", "title", "overview", "rating", "details") || !validTitle(t) || r.Key != t.Identity.Key() {
				return bad()
			}
		case "season":
			var t Season
			if decode(r.Data, &t) != nil || !requiredKeys(r.Data, "show_id", "season", "name", "episodes") || t.ShowID <= 0 || t.Number < 0 || r.Key != fmt.Sprintf("tv:%d:%d", t.ShowID, t.Number) || len(t.Episodes) > 1000 {
				return bad()
			}
			ep := map[int]bool{}
			for _, e := range t.Episodes {
				if e.Season != t.Number || e.Number < 1 || e.Runtime < 0 || !validArtPath(e.Still) || ep[e.Number] {
					return bad()
				}
				ep[e.Number] = true
			}
		case "shelf":
			var t Shelf
			if decode(r.Data, &t) != nil || !requiredKeys(r.Data, "id", "heading", "titles") || t.ID != r.Key || !validTitles(t.Titles) {
				return bad()
			}
		case "search":
			var t []Title
			if decode(r.Data, &t) != nil || !validTitles(t) {
				return bad()
			}
		case "cross_ids":
			var t struct {
				IMDb string `json:"imdb_id,omitempty"`
				MAL  int64  `json:"mal_id,omitempty"`
			}
			if decode(r.Data, &t) != nil || t.MAL < 0 || (t.IMDb != "" && !imdbPattern.MatchString(t.IMDb)) {
				return bad()
			}
			if !identityKey(r.Key) {
				return bad()
			}
		case "recommendations":
			var t Recommendations
			if !safeID(r.Key) || decode(r.Data, &t) != nil || !requiredKeys(r.Data, "fingerprint", "pool", "sections") || t.Fingerprint == "" || !validTitles(t.Pool) {
				return bad()
			}
			for _, v := range t.Sections {
				if !validTitles(v.Titles) {
					return bad()
				}
			}
		case "artwork":
			var a artwork
			if !artKey.MatchString(r.Key) || decode(r.Data, &a) != nil || len(a.Bytes) > 8<<20 || len(a.Bytes) == 0 {
				return bad()
			}
			c, format, e := image.DecodeConfig(bytes.NewReader(a.Bytes))
			if e != nil || c.Width <= 0 || c.Height <= 0 || c.Width > 8192 || c.Height > 8192 || int64(c.Width)*int64(c.Height) > 16000000 {
				return bad()
			}
			expected := map[string]string{"jpeg": "image/jpeg", "png": "image/png", "webp": "image/webp"}[format]
			if expected == "" || expected != a.MIME {
				return bad()
			}
			if _, _, e = image.Decode(bytes.NewReader(a.Bytes)); e != nil {
				return bad()
			}
		default:
			return bad()
		}
	}
	return nil
}
func identityKey(k string) bool {
	p := strings.Split(k, ":")
	if len(p) != 2 {
		return false
	}
	n, e := strconv.ParseInt(p[1], 10, 64)
	return e == nil && (Identity{p[0], n}).Valid()
}
func (s *Service) SyncSnapshot(ctx context.Context) error {
	b, e := s.private(ctx, "catalog", "GET", "/internal/v1/snapshot", nil, "", 192<<20)
	if e != nil {
		return e
	}
	var next Snapshot
	if e = decode(b, &next); e != nil {
		return e
	}
	if e = validateSnapshot(next); e != nil {
		return e
	}
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	if next.Revision < s.snapshot.Revision || next.Exported.Before(s.snapshot.Exported) {
		return &fault{502, "catalog_revision_regression"}
	}
	_, e = s.db.ExecContext(ctx, "INSERT INTO snapshot VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET revision=excluded.revision,data=excluded.data", next.Revision, b)
	if e != nil {
		return e
	}
	s.snapshot = next
	return nil
}
func (s *Service) record(kind, key string) (Record, bool) {
	s.snapMu.RLock()
	defer s.snapMu.RUnlock()
	for _, r := range s.snapshot.Records {
		if r.Kind == kind && r.Key == key {
			if time.Now().After(r.Expires) {
				r.State = "stale"
			}
			return r, true
		}
	}
	return Record{}, false
}
func (s *Service) records(kind string) []Record {
	s.snapMu.RLock()
	defer s.snapMu.RUnlock()
	out := []Record{}
	for _, r := range s.snapshot.Records {
		if r.Kind == kind {
			if time.Now().After(r.Expires) {
				r.State = "stale"
			}
			out = append(out, r)
		}
	}
	return out
}
func publicTitle(t Title) Title {
	t.Poster = strings.Replace(t.Poster, "/internal/v1/artwork/", "/api/v1/artwork/", 1)
	t.Backdrop = strings.Replace(t.Backdrop, "/internal/v1/artwork/", "/api/v1/artwork/", 1)
	return t
}
func publicTitles(t []Title) []Title {
	out := make([]Title, 0, len(t))
	for _, v := range t {
		out = append(out, publicTitle(v))
	}
	return out
}
func publicRecord(r Record) Record {
	switch r.Kind {
	case "title":
		var t Title
		_ = decode(r.Data, &t)
		r.Data = marshal(publicTitle(t))
	case "season":
		var t Season
		_ = decode(r.Data, &t)
		for i := range t.Episodes {
			t.Episodes[i].Still = strings.Replace(t.Episodes[i].Still, "/internal/v1/artwork/", "/api/v1/artwork/", 1)
		}
		r.Data = marshal(t)
	case "shelf":
		var t Shelf
		_ = decode(r.Data, &t)
		t.Titles = publicTitles(t.Titles)
		r.Data = marshal(t)
	case "search":
		var t []Title
		_ = decode(r.Data, &t)
		r.Data = marshal(publicTitles(t))
	}
	return r
}
func (s *Service) cachedTitle(i Identity) Title {
	r, ok := s.record("title", i.Key())
	if !ok {
		return Title{Identity: i}
	}
	var t Title
	_ = decode(r.Data, &t)
	return t
}
func (s *Service) catalogRead(w http.ResponseWriter, r *http.Request) error {
	p := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/")
	var rec Record
	var ok bool
	if len(p) == 3 && p[0] == "titles" {
		n, e := strconv.ParseInt(p[2], 10, 64)
		if e != nil {
			return bad()
		}
		i := Identity{p[1], n}
		if !i.Valid() {
			return bad()
		}
		_ = s.queueRefresh(r.Context(), map[string]any{"kind": "title", "type": i.Type, "id": i.ID})
		rec, ok = s.record("title", i.Key())
	} else if len(p) >= 4 && p[0] == "tv" && p[2] == "seasons" {
		n, e := strconv.ParseInt(p[1], 10, 64)
		season, se := strconv.Atoi(p[3])
		if e != nil || se != nil || n <= 0 || season < 0 || season > 1000 {
			return bad()
		}
		_ = s.queueRefresh(r.Context(), map[string]any{"kind": "season", "type": "tv", "id": n, "season": season})
		rec, ok = s.record("season", fmt.Sprintf("tv:%d:%d", n, season))
		if len(p) == 6 && p[4] == "episodes" {
			ep, e := strconv.Atoi(p[5])
			if e != nil || ep < 1 {
				return bad()
			}
			if !ok {
				return missing()
			}
			var sn Season
			_ = decode(publicRecord(rec).Data, &sn)
			for _, v := range sn.Episodes {
				if v.Number == ep {
					write(w, 200, map[string]any{"show_id": n, "data": v, "state": rec.State, "updated_at": rec.Updated, "expires_at": rec.Expires})
					return nil
				}
			}
			return missing()
		}
		if len(p) != 4 {
			return missing()
		}
	} else if len(p) == 1 && p[0] == "browse" {
		_ = s.queueRefresh(r.Context(), map[string]any{"kind": "browse"})
		out := s.records("shelf")
		for i := range out {
			out[i] = publicRecord(out[i])
		}
		write(w, 200, map[string]any{"records": out})
		return nil
	} else if len(p) == 1 && p[0] == "search" {
		q := r.URL.Query().Get("q")
		if len(q) == 0 || len(q) > 200 || strings.TrimSpace(q) == "" || strings.ContainsAny(q, "\x00\r\n") {
			return bad()
		}
		offset, e := parseOffset(r)
		if e != nil {
			return e
		}
		_ = s.queueRefresh(r.Context(), map[string]any{"kind": "search", "query": q})
		matches := []Record{}
		for _, v := range s.records("title") {
			var t Title
			_ = decode(v.Data, &t)
			if strings.Contains(strings.ToLower(t.Title), strings.ToLower(q)) {
				matches = append(matches, publicRecord(v))
			}
		}
		sort.Slice(matches, func(i, j int) bool { return matches[i].Key < matches[j].Key })
		start := min(offset, len(matches))
		end := min(start+50, len(matches))
		write(w, 200, map[string]any{"scope": "cached_titles", "records": matches[start:end], "offset": offset, "limit": 50})
		return nil
	} else if len(p) == 1 && p[0] == "search-results" {
		q := r.URL.Query().Get("q")
		if len(q) == 0 || len(q) > 200 || strings.TrimSpace(q) == "" || strings.ContainsAny(q, "\x00\r\n") {
			return bad()
		}
		rec, ok = s.record("search", q)
	} else if len(p) == 3 && p[0] == "artwork" {
		key := p[1] + "/" + p[2]
		if !artKey.MatchString(key) {
			return bad()
		}
		rec, ok = s.record("artwork", key)
		if !ok {
			return missing()
		}
		var a artwork
		_ = decode(rec.Data, &a)
		w.Header().Set("Content-Type", a.MIME)
		w.Header().Set("X-Catalog-State", rec.State)
		w.Header().Set("ETag", fmt.Sprintf(`"%d"`, rec.Revision))
		w.WriteHeader(200)
		_, _ = w.Write(a.Bytes)
		return nil
	} else {
		return missing()
	}
	if !ok {
		return missing()
	}
	write(w, 200, publicRecord(rec))
	return nil
}
func (s *Service) recommendations(w http.ResponseWriter, r *http.Request, p Profile) error {
	rec, ok := s.record("recommendations", p.ID)
	if !ok {
		return missing()
	}
	var t Recommendations
	_ = decode(rec.Data, &t)
	category := r.URL.Query().Get("type")
	if category == "" {
		category = "all"
	}
	if category != "all" && category != "movie" && category != "tv" {
		return bad()
	}
	t = recommendationView(t, category, p.Preferences.Format)
	var version, synced int64
	if e := s.db.QueryRowContext(r.Context(), "SELECT version,synced FROM profiles WHERE id=?", p.ID).Scan(&version, &synced); e != nil {
		return e
	}
	in, _, deriveErr := s.derived(r.Context(), p.ID)
	if deriveErr != nil {
		return deriveErr
	}
	if version != synced || t.Fingerprint != in.fingerprint() {
		rec.State = "stale"
	}
	rec.Key = "profile"
	rec.Data = marshal(t)
	write(w, 200, rec)
	return nil
}
func (s *Service) home(w http.ResponseWriter, r *http.Request, p Profile) error {
	out := s.records("shelf")
	for i := range out {
		out[i] = publicRecord(out[i])
	}
	var recommendation any
	if rec, ok := s.record("recommendations", p.ID); ok {
		var t Recommendations
		_ = decode(rec.Data, &t)
		t = recommendationView(t, "all", p.Preferences.Format)
		in, _, e := s.derived(r.Context(), p.ID)
		if e != nil {
			return e
		}
		if t.Fingerprint != in.fingerprint() {
			rec.State = "stale"
		}
		rec.Key = "profile"
		rec.Data = marshal(t)
		recommendation = rec
	}
	write(w, 200, map[string]any{"shelves": out, "recommendations": recommendation})
	return nil
}
func (s *Service) queueRefresh(ctx context.Context, job any) error {
	return transaction(ctx, s.db, func(tx *sql.Tx) error {
		var n int
		if e := tx.QueryRow("SELECT count(*) FROM refresh_jobs").Scan(&n); e != nil {
			return e
		}
		if n >= 256 {
			return nil
		}
		b := marshal(job)
		_, e := tx.Exec("INSERT OR IGNORE INTO refresh_jobs VALUES(?,?)", hash(string(b)), b)
		return e
	})
}
