package ckit

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sync"
	"text/template"
	"time"

	"github.com/gorilla/mux"
	"github.com/icholy/replace"
	"github.com/jmoiron/sqlx"
	"github.com/klauspost/compress/zstd"
	"github.com/segmentio/encoding/json"
	"github.com/slub/labe/go/ckit/cache"
	"github.com/slub/labe/go/ckit/set"
	"github.com/thoas/stats"
	"golang.org/x/text/transform"
)

var bufPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

var snippetPool = sync.Pool{
	New: func() interface{} {
		return new(Snippet)
	},
}

// Snippet is a small piece of index metadata used for institution filter.
type Snippet struct {
	Institution []string `json:"institution"`
}

// Server wraps three data sources required for index and citation data fusion.
// The IdentifierDatabase is a map from local identifier (e.g. 0-1238201) to
// DOI, the OciDatabase contains citing and cited relationships from OCI/COCI
// citation corpus and IndexData allows to fetch a metadata blob from a backing store.
//
// A performance data point: On a 8 core 16G RAM machine we can keep a
// sustained load of about 12K SQL qps, 150MB/s reads off disk. Total size of
// databases involved at about 224GB plus 7 GB cache (ie. at most 6% of the
// data can be held in memory at any given time).
//
// Under load requesting the most costly (and large) 150K docs the server will
// hover at around 10% (of 16GB) RAM.
type Server struct {
	// IdentifierDatabase maps local ids to DOI. The expected schema documented
	// here: https://github.com/miku/labe/tree/main/go/ckit#makta
	//
	// 0-025152688     10.1007/978-3-476-03951-4
	// 0-025351737     10.13109/9783666551536
	// 0-024312134     10.1007/978-1-4612-1116-7
	// 0-025217100     10.1007/978-3-322-96667-4
	// ...
	IdentifierDatabase *sqlx.DB
	// OciDatabase contains DOI to DOI mappings representing a citation
	// relationship. The expected schema documented here:
	// https://github.com/miku/labe/tree/main/go/ckit#makta
	//
	// 10.1002/9781119393351.ch1       10.1109/icelmach.2012.6350005
	// 10.1002/9781119393351.ch1       10.1115/detc2011-48151
	// 10.1002/9781119393351.ch1       10.1109/ical.2009.5262972
	// 10.1002/9781119393351.ch1       10.1109/cdc.2013.6760196
	// ...
	OciDatabase *sqlx.DB
	// IndexData allows to fetch a metadata blob given an identifier. This is
	// an interface that in the past has been implemented by types wrapping
	// microblob, SOLR and sqlite3, as well as a FetchGroup, that allows to
	// query multiple backends. We settled on sqlite3 and FetchGroup, the other
	// implementation are now gone.
	//
	// dswarm-126-ZnR0aG9zdHdlc3RsaX...   {"id":"dswarm-126-ZnR0aG9zdHdlc3RsaXBwZ...
	// dswarm-126-ZnR0aG9zdHdlc3RsaX...   {"id":"dswarm-126-ZnR0aG9zdHdlc3RsaXBwZ...
	// dswarm-126-ZnR0dW11ZW5jaGVuOm...   {"id":"dswarm-126-ZnR0dW11ZW5jaGVuOm9ha...
	// dswarm-126-ZnR0dW11ZW5jaGVuOm...   {"id":"dswarm-126-ZnR0dW11ZW5jaGVuOm9ha...
	// ...
	IndexData Fetcher
	// Router to register routes on.
	Router *mux.Router
	// StopWatchEnabled enabled the stopwatch, a builtin, simplistic request tracer.
	StopWatchEnabled bool
	// Cache for expensive items.
	Cache *cache.Cache
	// CacheTriggerDuration determines which items to cache.
	CacheTriggerDuration time.Duration
	// Stats, like request counts and status codes.
	Stats *stats.Stats
}

// Map is a generic lookup table. We use it together with sqlite3. This
// corresponds to the format generated by the makta command line tool:
// https://github.com/miku/labe/tree/main/go/ckit#makta.
type Map struct {
	Key   string `db:"k"`
	Value string `db:"v"`
}

// ErrorMessage from failed requests.
type ErrorMessage struct {
	Status int   `json:"status,omitempty"`
	Err    error `json:"err,omitempty"`
}

// Response contains a subset of index data fused with citation data. Citing
// and cited documents are raw bytes, but typically will contain JSON. For
// unmatched docs, we only transmit the DOI, e.g. {"doi_str_mv": "10.12/34"}.
type Response struct {
	ID        string            `json:"id,omitempty"`
	DOI       string            `json:"doi,omitempty"`
	Citing    []json.RawMessage `json:"citing,omitempty"`
	Cited     []json.RawMessage `json:"cited,omitempty"`
	Unmatched struct {
		Citing []json.RawMessage `json:"citing,omitempty"`
		Cited  []json.RawMessage `json:"cited,omitempty"`
	} `json:"unmatched,omitempty"`
	Extra struct {
		UnmatchedCitingCount int     `json:"unmatched_citing_count"`
		UnmatchedCitedCount  int     `json:"unmatched_cited_count"`
		CitingCount          int     `json:"citing_count"`
		CitedCount           int     `json:"cited_count"`
		Cached               bool    `json:"cached"`
		Took                 float64 `json:"took"` // seconds
		// Institution is set optionally, if the response has been tailored
		// towards the holdings of a given institution.
		Institution string `json:"institution,omitempty"`
	} `json:"extra,omitempty"`
}

// applyInstitutionFilter rearranges cited and citing documents in-place based
// on holdings of an institution (as found in the index data), given by its
// ISIL (ISO 15511). This will panic, if the index metadata is not valid JSON.
func (r *Response) applyInstitutionFilter(institution string) {
	var (
		citing []json.RawMessage
		cited  []json.RawMessage
	)
	for _, b := range r.Citing {
		v := snippetPool.Get().(*Snippet)
		if err := json.Unmarshal(b, v); err != nil {
			panic(fmt.Sprintf("internal data broken: %v", err))
		}
		if sliceContains(v.Institution, institution) {
			citing = append(citing, b)
		} else {
			r.Unmatched.Citing = append(r.Unmatched.Citing, b)
		}
		snippetPool.Put(v)
	}
	for _, b := range r.Cited {
		v := snippetPool.Get().(*Snippet)
		if err := json.Unmarshal(b, v); err != nil {
			panic(fmt.Sprintf("internal data broken: %v", err))
		}
		if sliceContains(v.Institution, institution) {
			cited = append(cited, b)
		} else {
			r.Unmatched.Cited = append(r.Unmatched.Cited, b)
		}
		snippetPool.Put(v)
	}
	r.Citing = citing
	r.Cited = cited
	r.updateCounts()
	r.Extra.Institution = institution
}

// updateCounts updates extra fields containing counts. Best called after the
// slice fields are not changed any more.
func (r *Response) updateCounts() {
	r.Extra.CitingCount = len(r.Citing)
	r.Extra.CitedCount = len(r.Cited)
	r.Extra.UnmatchedCitingCount = len(r.Unmatched.Citing)
	r.Extra.UnmatchedCitedCount = len(r.Unmatched.Cited)
}

// Routes sets up routes.
func (s *Server) Routes() {
	s.Router.HandleFunc("/", s.handleIndex()).Methods("GET")
	s.Router.HandleFunc("/cache", s.handleCacheInfo()).Methods("GET")
	s.Router.HandleFunc("/cache", s.handleCachePurge()).Methods("DELETE")
	s.Router.HandleFunc("/doi/{doi:.*}", s.handleDOI()).Methods("GET")
	s.Router.HandleFunc("/id/{id}", s.handleLocalIdentifier()).Methods("GET")
	s.Router.HandleFunc("/stats", s.handleStats()).Methods("GET")
}

// ServeHTTP turns the server into an HTTP handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Router.ServeHTTP(w, r)
}

// handleIndex handles the root route.
func (s *Server) handleIndex() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		docs := `
    ___       ___       ___       ___       ___
   /\__\     /\  \     /\  \     /\  \     /\  \
  /:/  /    /::\  \   /::\  \   /::\  \   /::\  \
 /:/__/    /::\:\__\ /::\:\__\ /::\:\__\ /:/\:\__\
 \:\  \    \/\::/  / \:\::/  / \:\:\/  / \:\/:/  /
  \:\__\     /:/  /   \::/  /   \:\/  /   \::/  /
   \/__/     \/__/     \/__/     \/__/     \/__/

Pid: {{ .PID }} | https://github.com/slub/labe

Available endpoints:

    /              GET
    /cache         DELETE
    /cache         GET
    /doi/{doi}     GET
    /id/{id}       GET
    /stats         GET

Examples:

  http://{{ .Hostport }}/id/ai-49-aHR0cDovL2R4LmRvaS5vcmcvMTAuMTA3My9wbmFzLjg1LjguMjQ0NA
  http://{{ .Hostport }}/id/ai-49-aHR0cDovL2R4LmRvaS5vcmcvMTAuMTAwMS9qYW1hLjI4Mi4xNi4xNTE5
  http://{{ .Hostport }}/id/ai-49-aHR0cDovL2R4LmRvaS5vcmcvMTAuMTAwNi9qbXJlLjE5OTkuMTcxNQ
  http://{{ .Hostport }}/id/ai-49-aHR0cDovL2R4LmRvaS5vcmcvMTAuMTE3Ny8xMDQ5NzMyMzA1Mjc2Njg3
  http://{{ .Hostport }}/id/ai-49-aHR0cDovL2R4LmRvaS5vcmcvMTAuMTIxMC9qYy4yMDExLTAzODU
  http://{{ .Hostport }}/id/ai-49-aHR0cDovL2R4LmRvaS5vcmcvMTAuMTIxNC9hb3MvMTE3NjM0Nzk2Mw
  http://{{ .Hostport }}/id/ai-49-aHR0cDovL2R4LmRvaS5vcmcvMTAuMjMwNy8yMDk1NTIx

`
		t := template.Must(template.New("index").Parse(docs))
		err := t.Execute(w, struct {
			PID      int
			Hostport string
		}{
			PID:      os.Getpid(),
			Hostport: r.Host,
		})
		if err != nil {
			httpErrLog(w, http.StatusInternalServerError, err)
		}
	}
}

// handleCacheInfo returns the number of currently cached items.
func (s *Server) handleCacheInfo() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Cache != nil {
			count, err := s.Cache.ItemCount()
			if err != nil {
				httpErrLog(w, http.StatusInternalServerError, err)
				return
			}
			err = json.NewEncoder(w).Encode(map[string]interface{}{
				"count": count,
				"path":  s.Cache.Path,
			})
			if err != nil {
				httpErrLog(w, http.StatusInternalServerError, err)
				return
			}
		}
	}
}

// handleCachePurge empties the cache.
func (s *Server) handleCachePurge() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Cache == nil {
			return
		}
		if err := s.Cache.Flush(); err != nil {
			httpErrLog(w, http.StatusInternalServerError, err)
			return
		} else {
			log.Println("flushed cached")
		}
	}
}

// handleStats renders a JSON overview of server metrics.
func (s *Server) handleStats() http.HandlerFunc {
	s.Stats.MetricsCounts = make(map[string]int)
	s.Stats.MetricsTimers = make(map[string]time.Time)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(s.Stats.Data()); err != nil {
			httpErrLog(w, http.StatusInternalServerError, err)
			return
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
		err := s.IdentifierDatabase.GetContext(ctx, &response.ID, "SELECT k FROM map WHERE v = ?", response.DOI)
		if err != nil {
			switch {
			case err == context.Canceled:
				log.Printf("handle doi: %v", err)
			default:
				http.Error(w, `{"msg": "no id found", "status": 404}`, http.StatusNotFound)
			}
		} else {
			target := fmt.Sprintf("/id/%s", response.ID)
			w.Header().Set("Content-Type", "text/plain") // disable http snippet
			http.Redirect(w, r, target, http.StatusTemporaryRedirect)
		}
	}
}

// handleLocalIdentifier does all the lookups and assembles a JSON response.
func (s *Server) handleLocalIdentifier() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// (0) check for cached value
		// (1) resolve id to doi
		// (2) lookup related doi via oci
		// (3) resolve doi to ids
		// (4) lookup all ids
		// (5) include unmatched ids
		// (6) assemble result
		// (7) cache, if request was expensive
		// (8) optional: apply institution filter
		// (9) send response
		var (
			ctx          = r.Context()
			started      = time.Now()
			vars         = mux.Vars(r)
			ids          []Map
			outbound     = set.New()
			inbound      = set.New()
			matched      []string
			unmatchedSet = set.New()
			response     = &Response{
				ID: vars["id"],
			}
			sw StopWatch
			// Experimental, hacky support for limiting results to the documents of
			// a particular institution, given as it appears in the "institution"
			// field of the index data, e.g. "DE-14".
			isil = r.URL.Query().Get("i")
		)
		sw.SetEnabled(s.StopWatchEnabled)
		sw.Recordf("[%s] started query: %s", isil, response.ID)
		// Ganz sicher application/json.
		w.Header().Add("Content-Type", "application/json")
		// (0) Check cache first.
		if s.Cache != nil {
			t := time.Now()
			b, err := s.Cache.Get(response.ID)
			if err == nil {
				sw.Recordf("retrieved value (%db) from cache", len(b))
				r, err := zstd.NewReader(bytes.NewReader(b))
				if err != nil {
					httpErrLogf(w, http.StatusInternalServerError, "cache decompress: %w", err)
					return
				}
				took := fmt.Sprintf(`"took":%f`, time.Since(started).Seconds())
				replacer := transform.NewReader(r, replace.RegexpString(regexp.MustCompile(`"took":[0-9.]+`), took))
				switch {
				case isil != "":
					var resp Response
					if err := json.NewDecoder(replacer).Decode(&resp); err != nil {
						httpErrLogf(w, http.StatusInternalServerError, "cache json decode: %w", err)
						return
					}
					resp.applyInstitutionFilter(isil)
					sw.Record("applied institution filter")
					if err := json.NewEncoder(w).Encode(resp); err != nil {
						httpErrLogf(w, http.StatusInternalServerError, "encode: %w", err)
						return
					}
				default:
					if _, err := io.Copy(w, replacer); err != nil {
						httpErrLogf(w, http.StatusInternalServerError, "cache copy: %w", err)
						return
					}
				}
				r.Close()
				s.Stats.MeasureSinceWithLabels("cache_hit", t, nil)
				sw.Record("sent cached value")
				sw.LogTable()
				return
			}
		}
		// (1) Get the DOI for the local id; or get out.
		t := time.Now()
		err := s.IdentifierDatabase.GetContext(ctx, &response.DOI, "SELECT v FROM map WHERE k = ?", response.ID)
		if err != nil {
			switch {
			case err == sql.ErrNoRows:
				log.Printf("doi lookup (%s): %v", response.ID, err)
				httpErrLogf(w, http.StatusNotFound, "doi lookup (%s): %w", response.ID, err)
			case err == context.Canceled:
				log.Printf("doi lookup (%s): %v", response.ID, err)
			default:
				httpErrLogf(w, http.StatusInternalServerError, "select id: %w", err)
			}
			return
		}
		s.Stats.MeasureSinceWithLabels("sql_query", t, nil)
		sw.Recordf("found doi: %s", response.DOI)
		// (2) Get outbound and inbound edges.
		citing, cited, err := s.edges(ctx, response.DOI)
		if err != nil {
			switch {
			case err == context.Canceled:
				log.Println(err)
			default:
				httpErrLogf(w, http.StatusInternalServerError, "edges: %w", err)
			}
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
		ds := outbound.Union(inbound)
		if ds.IsEmpty() {
			log.Printf("no citations found: %s", response.ID)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// (4) Map relevant DOI back to local identifiers.
		if ids, err = s.mapToLocal(ctx, ds.Slice()); err != nil {
			switch {
			case err == context.Canceled:
				log.Println(err)
			default:
				httpErrLogf(w, http.StatusInternalServerError, "map: %w", err)
			}
			return
		}
		sw.Recordf("mapped %d dois back to ids", ds.Len())
		// (5) Here, we can find unmatched items, via DOI.
		for _, v := range ids {
			matched = append(matched, v.Value)
		}
		unmatchedSet = ds.Difference(set.FromSlice(matched))
		for k := range unmatchedSet {
			// We shortcut and do not use a proper JSON marshaller to save a
			// bit of time. TODO: may switch to proper JSON encoding, if other
			// parts are more optimized.
			b := []byte(fmt.Sprintf(`{"doi_str_mv": %q}`, k))
			switch {
			case outbound.Contains(k):
				response.Unmatched.Citing = append(response.Unmatched.Citing, b)
			case inbound.Contains(k):
				response.Unmatched.Cited = append(response.Unmatched.Cited, b)
			default:
				panic("cosmic rays detected (in-flight change of inbound or outbound values)")
			}
		}
		sw.Record("recorded unmatched ids")
		// (6) At this point, we need to assemble the result. For each
		// identifier we want the full metadata. We currently use an local
		// sqlite copy of the index data as this seems to be the fastest
		// option.
		//
		// This is agnostic to the index data content, it can contain
		// the full metadata record, or just a few fields.
		for _, v := range ids {
			t := time.Now()
			b, err := s.IndexData.Fetch(v.Key)
			s.Stats.MeasureSinceWithLabels("index_data_fetch", t, nil)
			if errors.Is(err, ErrBlobNotFound) {
				continue
			}
			if err != nil {
				httpErrLogf(w, http.StatusInternalServerError, "index data fetch: %w", err)
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
		// Finalize response.
		response.updateCounts()
		response.Extra.Took = time.Since(started).Seconds()
		// (7) Cache expensive results.
		if s.Cache != nil && time.Since(started) > s.CacheTriggerDuration {
			t := time.Now()
			response.Extra.Cached = true
			buf := bufPool.Get().(*bytes.Buffer)
			buf.Reset()
			// Wrap cache handling, so we can use defer to reclaim the buffer.
			wrap := func() error {
				defer bufPool.Put(buf)
				zw, err := zstd.NewWriter(buf)
				if err != nil {
					return fmt.Errorf("cache compress: %w", err)
				}
				// We cache the unfiltered response (otherwise the cache would
				// waste disk space).
				if err := json.NewEncoder(zw).Encode(response); err != nil {
					return fmt.Errorf("cache json encode: %w", err)
				}
				if err := zw.Close(); err != nil {
					return fmt.Errorf("cache close: %w", err)
				}
				if err := s.Cache.Set(response.ID, buf.Bytes()); err != nil {
					if err == cache.ErrReadOnly {
						return nil
					} else {
						// TODO: we do not need to fail, if cache fails
						return fmt.Errorf("failed to cache value for %s: %v", response.ID, err)
					}
				}
				s.Stats.MeasureSinceWithLabels("cached", t, nil)
				sw.Record("cached value")
				return nil
			}
			if err := wrap(); err != nil {
				httpErrLog(w, http.StatusInternalServerError, err)
				return
			}
		}
		// (8) Optional: Apply institution filter.
		if isil != "" {
			response.applyInstitutionFilter(isil)
			sw.Record("applied institution filter")
		}
		// (9) Send response.
		if err := json.NewEncoder(w).Encode(response); err != nil {
			httpErrLogf(w, http.StatusInternalServerError, "encode: %w", err)
			return
		}
		sw.Record("sent response")
		sw.LogTable()
	}
}

// Ping returns an error, if any of the datastores is not available.
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

// edges returns citing (outbound) and cited (inbound) edges for a given DOI.
func (s *Server) edges(ctx context.Context, doi string) (citing, cited []Map, err error) {
	t := time.Now()
	if err := s.OciDatabase.SelectContext(
		ctx, &citing, "SELECT * FROM map WHERE k = ?", doi); err != nil {
		return nil, nil, err
	}
	s.Stats.MeasureSinceWithLabels("sql_query", t, nil)
	t = time.Now()
	if err := s.OciDatabase.SelectContext(
		ctx, &cited, "SELECT * FROM map WHERE v = ?", doi); err != nil {
		return nil, nil, err
	}
	s.Stats.MeasureSinceWithLabels("sql_query", t, nil)
	return citing, cited, nil
}

// mapToLocal takes a list of DOI and returns a slice of Maps containing the
// local id and DOI.
func (s *Server) mapToLocal(ctx context.Context, dois []string) (ids []Map, err error) {
	var (
		t     time.Time
		query string
		args  []interface{}
		// sqlite has a limit on the variable count, which at most is 999; it may
		// lead to "too many SQL variables", SQLITE_LIMIT_VARIABLE_NUMBER (default:
		// 999; https://www.daemon-systems.org/man/sqlite3_bind_blob.3.html).
		size = 500 // Anything between 1 and 999.
	)
	for _, batch := range batchedStrings(dois, size) {
		t = time.Now()
		query, args, err = sqlx.In("SELECT * FROM map WHERE v IN (?)", batch)
		if err != nil {
			return nil, fmt.Errorf("query (%d): %v", len(dois), err)
		}
		var result []Map
		query = s.IdentifierDatabase.Rebind(query)
		err = s.IdentifierDatabase.SelectContext(ctx, &result, query, args...)
		if err != nil {
			return nil, fmt.Errorf("select (%d): %v", len(dois), err)
		}
		s.Stats.MeasureSinceWithLabels("sql_query", t, nil)
		for _, r := range result {
			ids = append(ids, r)
		}
	}
	return ids, nil
}

// batchedStrings batches one string slice into a potentially smaller number of
// strings slices with size at most n.
func batchedStrings(ss []string, n int) (result [][]string) {
	b, e := 0, n
	for {
		if len(ss) <= e {
			result = append(result, ss[b:])
			return
		} else {
			result = append(result, ss[b:e])
			b, e = e, e+n
		}
	}
	return
}

// sliceContains returns true, if a string slice contains a given value.
func sliceContains(ss []string, v string) bool {
	for _, s := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// httpErrLogf is a log formatting helper.
func httpErrLogf(w http.ResponseWriter, status int, s string, a ...interface{}) {
	httpErrLog(w, status, fmt.Errorf(s, a...))
}

// httpErrLogStatus returns an error to the client and logs the error.
func httpErrLog(w http.ResponseWriter, status int, err error) {
	log.Printf("failed [%d]: %v", status, err)
	b, err := json.Marshal(&ErrorMessage{
		Status: status,
		Err:    err,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Error(w, string(b), status)
}
