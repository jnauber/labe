package spindel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/jmoiron/sqlx"
	"github.com/miku/labe/tools/spindel/set"
	"github.com/patrickmn/go-cache"
	"github.com/segmentio/encoding/json"
)

// Server wraps three data sources required for index and citation data fusion.
// The IdentifierDatabase is a map from local identifier (e.g. 0-1238201) to
// DOI, the OciDatabase contains citing and cited relationsships from OCI/COCI
// citation corpus and IndexData allows to fetch a metadata blob from a
// service, e.g. a key value store like microblob, sqlite3, solr, elasticsearch
// or in memory store.
type Server struct {
	IdentifierDatabase   *sqlx.DB
	OciDatabase          *sqlx.DB
	IndexData            Fetcher
	Router               *mux.Router
	StopWatchEnabled     bool
	CacheEnabled         bool
	CacheTriggerDuration time.Duration
	CacheTTL             time.Duration
	cache                *cache.Cache
}

// Map is a generic lookup table. We use it together with sqlite3.
type Map struct {
	Key   string `db:"k"`
	Value string `db:"v"`
}

// Response contains a subset of index data fused with citation data.
// Citing and cited documents are unparsed. For unmatched docs, we keep
// only transmit the DOI, e.g. as {"doi": "10.123/123"}.
type Response struct {
	ID        string            `json:"id"`
	DOI       string            `json:"doi"`
	Citing    []json.RawMessage `json:"citing,omitempty"`
	Cited     []json.RawMessage `json:"cited,omitempty"`
	Unmatched struct {
		Citing []json.RawMessage `json:"citing,omitempty"`
		Cited  []json.RawMessage `json:"cited,omitempty"`
	} `json:"unmatched"`
	Extra struct {
		Took                 float64 `json:"took"`
		UnmatchedCitingCount int     `json:"unmatched_citing_count"`
		UnmatchedCitedCount  int     `json:"unmatched_cited_count"`
		CitingCount          int     `json:"citing_count"`
		CitedCount           int     `json:"cited_count"`
		Cached               bool    `json:"cached"`
	} `json:"extra"`
}

// updateCounts updates extra fields containing counts.
func (r *Response) updateCounts() {
	r.Extra.CitingCount = len(r.Citing)
	r.Extra.CitedCount = len(r.Cited)
	r.Extra.UnmatchedCitingCount = len(r.Unmatched.Citing)
	r.Extra.UnmatchedCitedCount = len(r.Unmatched.Cited)
}

// Routes sets up route. TODO: we want a direct DOI route as well.
func (s *Server) Routes() {
	s.Router.HandleFunc("/", s.handleIndex())
	s.Router.HandleFunc("/cache/size", s.handleCacheSize())
	s.Router.HandleFunc("/cache", s.handleCachePurge()).Methods("DELETE")
	s.Router.HandleFunc("/id/{id}", s.handleLocalIdentifier())
	s.Router.HandleFunc("/doi/{doi:.*}", s.handleDOI())
}

// ServeHTTP turns the server into an HTTP handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Router.ServeHTTP(w, r)
}

// edges returns citing (outbound) and citing (inbound) edges for a given DOI.
func (s *Server) edges(ctx context.Context, doi string) (citing, cited []Map, err error) {
	if err := s.OciDatabase.SelectContext(ctx, &citing,
		"SELECT * FROM map WHERE k = ?", doi); err != nil {
		return nil, nil, err
	}
	if err := s.OciDatabase.SelectContext(ctx, &cited,
		"SELECT * FROM map WHERE v = ?", doi); err != nil {
		return nil, nil, err
	}
	return citing, cited, nil
}

// mapToLocal takes a list of DOI and returns a slice of Maps containing the
// local id and DOI.
func (s *Server) mapToLocal(ctx context.Context, dois []string) (ids []Map, err error) {
	query, args, err := sqlx.In("SELECT * FROM map WHERE v IN (?)", dois)
	if err != nil {
		return nil, err
	}
	query = s.IdentifierDatabase.Rebind(query)
	if err := s.IdentifierDatabase.SelectContext(ctx, &ids, query, args...); err != nil {
		return nil, err
	}
	return ids, nil
}

func (s *Server) handleIndex() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// TODO: Render docs.
		fmt.Fprintf(w, "spindel")
	}
}

// handleCacheSize returns the number of currently cached items.
func (s *Server) handleCacheSize() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.CacheEnabled {
			err := json.NewEncoder(w).Encode(map[string]interface{}{
				"count": s.cache.ItemCount(),
			})
			if err != nil {
				httpErrLog(w, err)
				return
			}
		}
	}
}

// handleCachePurge empties the cache.
func (s *Server) handleCachePurge() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.CacheEnabled {
			s.cache.Flush()
			log.Println("flushed cached")
		}
	}
}

// handleDOI currently only redirects to the local id handler.
func (s *Server) handleDOI() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var (
			ctx      = r.Context()
			vars     = mux.Vars(r)
			response = &Response{
				DOI: vars["doi"],
			}
		)
		if err := s.IdentifierDatabase.GetContext(ctx, &response.ID,
			"SELECT k FROM map WHERE v = ?", response.DOI); err != nil {
			httpErrLog(w, err)
			return
		}
		target := fmt.Sprintf("/id/%s", response.ID)
		w.Header().Set("Content-Type", "text/plain") // disable http snippet
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
	}
}

// handleLocalIdentifier does all the lookups, but that should elsewhere, in a more
// testable place. Also, reuse some existing stats library. Also TODO: optimize
// backend requests and think up schema for delivery.
func (s *Server) handleLocalIdentifier() http.HandlerFunc {
	// We only care about caching here. TODO: we could use a closure for the
	// cache here (and not store it directly on the server).
	if s.CacheEnabled {
		s.cache = cache.New(72*time.Hour, s.CacheTTL)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// (1) resolve id to doi
		// (2) lookup related doi via oci
		// (3) resolve doi to ids
		// (4) lookup all ids
		// (5) include unmatched ids
		// (6) assemble result
		var (
			ctx          = r.Context()
			started      = time.Now()
			vars         = mux.Vars(r)
			ids          []Map // local identifiers
			outbound     = set.New()
			inbound      = set.New()
			matched      []string    // dangling DOI that have no local id
			unmatchedSet = set.New() // all unmatched doi
			response     = &Response{
				ID: vars["id"], // the local identifier
			}
			sw StopWatch
		)
		sw.SetEnabled(s.StopWatchEnabled)
		sw.Recordf("started query for: %s", vars["id"])
		// (0) Check cache first.
		if s.CacheEnabled {
			v, found := s.cache.Get(vars["id"])
			if found {
				if b, ok := v.([]byte); !ok {
					s.cache.Delete(vars["id"])
					log.Printf("[cache] removed bogus cache value")
				} else {
					sw.Record("retrieved value from cache")
					if _, err := w.Write(b); err != nil {
						httpErrLog(w, err)
						return
					}
					sw.Record("used cached value")
					sw.LogTable()
					return
				}
			}
		}
		// (1) Get the DOI for the local id; or get out.
		if err := s.IdentifierDatabase.GetContext(ctx, &response.DOI,
			"SELECT v FROM map WHERE k = ?", response.ID); err != nil {
			httpErrLog(w, err)
			return
		}
		sw.Recordf("found doi for id: %s", response.DOI)
		// (2) Get outbound and inbound edges.
		citing, cited, err := s.edges(ctx, response.DOI)
		if err != nil {
			httpErrLog(w, err)
			return
		}
		sw.Recordf("found %d outbound and %d inbound edges", len(citing), len(cited))
		// (3) We want to collect the unique set of DOI to get the complete
		// indexed documents.
		for _, v := range citing {
			outbound.Add(v.Value)
		}
		for _, v := range cited {
			inbound.Add(v.Key)
		}
		ss := outbound.Union(inbound)
		if ss.IsEmpty() {
			// This is where the difference in the benchmark runs comes from,
			// e.g. 64860/100000; estimated ratio 64% of records with DOI will
			// have some reference information. TODO: dig a bit deeper.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// (4) Map relevant DOI back to local identifiers.
		if ids, err = s.mapToLocal(ctx, ss.Slice()); err != nil {
			httpErrLog(w, err)
			return
		}
		sw.Recordf("mapped %d dois back to ids", ss.Len())
		// (5) Here, we can find unmatched items, via DOI.
		for _, v := range ids {
			matched = append(matched, v.Value)
		}
		unmatchedSet = ss.Difference(set.FromSlice(matched))
		for k := range unmatchedSet {
			// We shortcut and do not use a proper JSON marshaller to save a
			// bit of time. TODO: may switch to proper JSON encoding, if other
			// parts are more optimized.
			b := []byte(fmt.Sprintf(`{"doi": %q}`, k))
			if outbound.Contains(k) {
				response.Unmatched.Cited = append(response.Unmatched.Cited, b)
			} else {
				response.Unmatched.Citing = append(response.Unmatched.Citing, b)
			}
		}
		sw.Record("recorded unmatched ids")
		// (6) At this point, we need to assemble the result. For each
		// identifier we want the full metadata. We use an local copy of the
		// index. We could also ask a live index here.
		for _, v := range ids {
			b, err := s.IndexData.Fetch(v.Key)
			if errors.Is(err, ErrBlobNotFound) {
				continue
			}
			if err != nil {
				httpErrLog(w, err)
				return
			}
			switch {
			case outbound.Contains(v.Value):
				response.Citing = append(response.Citing, b)
			case inbound.Contains(v.Value):
				response.Cited = append(response.Cited, b)
			}
		}
		sw.Recordf("fetched %d blob from index data store", len(ids))
		response.updateCounts()
		response.Extra.Took = time.Since(started).Seconds()
		// Ganz sicher application/json.
		w.Header().Add("Content-Type", "application/json")
		// (7) If this request was expensive, cache it.
		switch {
		case s.CacheEnabled && time.Since(started) > s.CacheTriggerDuration:
			response.Extra.Cached = true
			b, err := json.Marshal(response)
			if err != nil {
				httpErrLog(w, err)
				return
			}
			s.cache.Set(vars["id"], b, 8*time.Hour)
			if _, err := w.Write(b); err != nil {
				httpErrLog(w, err)
				return
			}
			sw.Record("encoded JSON and cached value")
			sw.LogTable()
		default:
			enc := json.NewEncoder(w)
			if err := enc.Encode(response); err != nil {
				httpErrLog(w, err)
				return
			}
			sw.Record("encoded JSON")
			sw.LogTable()
		}
	}
}

// Ping returns an error, if any of the datastores are not available.
func (s *Server) Ping() error {
	if err := s.IdentifierDatabase.Ping(); err != nil {
		return err
	}
	if err := s.OciDatabase.Ping(); err != nil {
		return err
	}
	if pinger, ok := s.IndexData.(Pinger); ok {
		if err := pinger.Ping(); err != nil {
			return fmt.Errorf("could not reach index data service: %w", err)
		}
	} else {
		log.Printf("index data service: unknown status")
	}
	return nil
}

// httpErrLogStatus logs the error and returns.
func httpErrLogStatus(w http.ResponseWriter, err error, status int) {
	log.Printf("failed [%d]: %v", status, err)
	http.Error(w, err.Error(), status)
}

// httpErrLog tries to infer an appropriate status code.
func httpErrLog(w http.ResponseWriter, err error) {
	var status = http.StatusInternalServerError
	if errors.Is(err, context.Canceled) {
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		status = http.StatusNotFound
	}
	httpErrLogStatus(w, err, status)
}
