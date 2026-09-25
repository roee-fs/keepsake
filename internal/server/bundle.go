// Bundle uploads: a gzipped tar of OKF files, read as `keepsake import` reads a directory.
package server

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	"github.com/roee-fs/keepsake/okf"
)

// Upload limits, vars so tests can lower them. ponytail: the whole bundle sits in memory; stream it if bundles outgrow these.
var (
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
