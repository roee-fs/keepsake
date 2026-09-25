// Bundle uploads: a gzipped tar of OKF files, read as `keepsake import` reads a directory.
package server

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"unicode"

	"github.com/roee-fs/keepsake/internal/store"
	"github.com/roee-fs/keepsake/okf"
)

// Upload limits, vars so tests can lower them. ponytail: the whole bundle sits in memory, so uploads run one at a time; stream them if bundles outgrow these.
var (
	uploads           = make(chan struct{}, 1)
	maxUpload   int64 = 32 << 20
	maxUnpacked int64 = 64 << 20
	maxFiles          = 20_000
	maxFile     int64 = 1 << 20
)

var errTooLarge = errors.New("bundle is too large")

// readBundle parses every OKF file in a gzipped tar as a concept under prefix.
// problems names each file that cannot be stored; err means the archive cannot be read at all.
func readBundle(body io.Reader, prefix string) (concepts []okf.Concept, problems []string, err error) {
	gz, err := gzip.NewReader(body)
	if err != nil {
		return nil, nil, fmt.Errorf("not gzip: %w", err)
	}
	// One byte past the limit, so reaching it is distinguishable from ending exactly on it.
	unpacked := &io.LimitedReader{R: gz, N: maxUnpacked + 1}
	tr := tar.NewReader(unpacked)
	seen := map[string]bool{}
	var materialized int64
	for {
		hdr, err := tr.Next()
		if unpacked.N <= 0 {
			return nil, nil, errTooLarge
		}
		if errors.Is(err, io.EOF) {
			return concepts, problems, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("not a tar archive: %w", err)
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		rel := strings.TrimSuffix(name, ".md")
		switch {
		case hdr.Typeflag == tar.TypeDir || hdr.Typeflag == tar.TypeXGlobalHeader:
			continue
		// ._ files are the resource forks macOS tar adds beside each file.
		case !strings.HasSuffix(name, ".md") || strings.HasPrefix(path.Base(name), "._") || okf.ReservedPaths[rel]:
			continue
		case !fs.ValidPath(name):
			problems = append(problems, name+": not a relative path inside the bundle")
			continue
		case hdr.Typeflag != tar.TypeReg:
			problems = append(problems, name+": not a regular file")
			continue
		case seen[name]:
			problems = append(problems, name+": appears twice")
			continue
		case hdr.Size > maxFile:
			problems = append(problems, fmt.Sprintf("%s: larger than %d bytes", name, maxFile))
			continue
		}
		seen[name] = true
		if len(seen) > maxFiles {
			return nil, nil, errTooLarge
		}
		text, err := io.ReadAll(tr)
		if unpacked.N <= 0 {
			return nil, nil, errTooLarge
		}
		if err != nil {
			return nil, nil, fmt.Errorf("not a tar archive: %w", err)
		}
		// Sparse entries expand without reading extra stream bytes, so bound decoded bytes too.
		materialized += int64(len(text))
		if materialized > maxUnpacked {
			return nil, nil, errTooLarge
		}
		c, err := okf.Parse(string(text), prefix+"/"+rel)
		if err == nil {
			err = okf.Storable(c)
		}
		if err != nil {
			problems = append(problems, name+": "+err.Error())
			continue
		}
		concepts = append(concepts, c)
	}
}

// replaceBundle answers PUT /bundle?prefix=P: the caller's concepts under P become exactly the uploaded bundle.
func replaceBundle(cs *store.ConceptStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := r.Context().Value(callerKey{}).(caller)
		if !ok {
			internalError(w, r, errors.New("no caller bound to /bundle"))
			return
		}
		prefix := r.URL.Query().Get("prefix")
		if !fs.ValidPath(prefix) || prefix == "." || strings.ContainsFunc(prefix, unicode.IsControl) {
			writeJSON(w, r, http.StatusUnprocessableEntity, detail{"prefix must be a relative concept path, such as docs/runbooks"})
			return
		}
		select {
		case uploads <- struct{}{}:
			defer func() { <-uploads }()
		default:
			w.Header().Set("Retry-After", "5")
			writeJSON(w, r, http.StatusServiceUnavailable, detail{"another bundle upload is in progress"})
			return
		}
		concepts, problems, err := readBundle(http.MaxBytesReader(w, r.Body, maxUpload), prefix)
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig) || errors.Is(err, errTooLarge):
			writeJSON(w, r, http.StatusRequestEntityTooLarge, detail{errTooLarge.Error()})
			return
		case err != nil:
			writeJSON(w, r, http.StatusBadRequest, detail{err.Error()})
			return
		case len(problems) > 0:
			writeJSON(w, r, http.StatusUnprocessableEntity, detail{problems})
			return
		case len(concepts) == 0:
			// An empty upload is far likelier a packaging mistake than a corpus that emptied.
			writeJSON(w, r, http.StatusUnprocessableEntity, detail{"bundle holds no concepts"})
			return
		}
		deleted, err := cs.ReplacePrefix(r.Context(), c.tenant, prefix, concepts, c.actor)
		if err != nil {
			internalError(w, r, err)
			return
		}
		// The prefix is not logged, since a path names a concept.
		slog.Info("bundle replaced", "tenant", c.tenant.String(), "actor", c.actor, "written", len(concepts), "deleted", deleted)
		writeJSON(w, r, http.StatusOK, map[string]int{"written": len(concepts), "deleted": deleted})
	}
}
