package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

type entry struct {
	name, body string
	typ        byte
}

func tarball(t *testing.T, entries ...entry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644}
		switch e.typ {
		case 0:
			hdr.Typeflag, hdr.Size = tar.TypeReg, int64(len(e.body))
		case tar.TypeSymlink:
			hdr.Linkname = "elsewhere.md"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func md(typ, body string) string { return "---\ntype: " + typ + "\n---\n" + body + "\n" }

func TestABundleBecomesConceptsUnderItsPrefix(t *testing.T) {
	concepts, problems, err := readBundle(tarball(t,
		entry{name: "./", typ: tar.TypeDir},
		entry{name: "./a.md", body: md("Doc", "a")},
		entry{name: "./sub/b.md", body: md("Doc", "[a](../a.md)")},
		entry{name: "./README.txt", body: "not a concept"},
		entry{name: "./index.md", body: "# Index"},
		entry{name: "./._a.md", body: "\x00\x05\x16\x07 resource fork"},
	), "docs")
	if err != nil || len(problems) > 0 {
		t.Fatalf("readBundle = %v, %v", problems, err)
	}
	var got []string
	for _, c := range concepts {
		got = append(got, c.Path)
	}
	if want := []string{"docs/a", "docs/sub/b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(concepts[1].Links, []string{"docs/a"}) {
		t.Fatalf("links = %v, want [docs/a]", concepts[1].Links)
	}
}

func TestEveryUnstorableFileIsReported(t *testing.T) {
	_, problems, err := readBundle(tarball(t,
		entry{name: "typeless.md", body: "no frontmatter"},
		entry{name: "nan.md", body: "---\ntype: Doc\nscore: .nan\n---\n"},
		entry{name: "dup.md", body: md("Doc", "1")},
		entry{name: "dup.md", body: md("Doc", "2")},
		entry{name: "link.md", typ: tar.TypeSymlink},
		entry{name: "../escape.md", body: md("Doc", "")},
		entry{name: "big.md", body: strings.Repeat("x", int(maxFile)+1)},
	), "docs")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"typeless.md: type is required",
		"nan.md: frontmatter holds NaN or Infinity, which JSON cannot store",
		"dup.md: appears twice",
		"link.md: not a regular file",
		"../escape.md: not a relative path inside the bundle",
		"big.md: larger than 1048576 bytes",
	}
	if !reflect.DeepEqual(problems, want) {
		t.Fatalf("problems = %q\nwant %q", problems, want)
	}
}

func TestAnArchivePastTheUnpackedLimitIsRefused(t *testing.T) {
	defer func(n int64) { maxUnpacked = n }(maxUnpacked)
	maxUnpacked = 4096
	_, _, err := readBundle(tarball(t, entry{name: "a.md", body: md("Doc", strings.Repeat("x", 8192))}), "docs")
	if !errors.Is(err, errTooLarge) {
		t.Fatalf("err = %v, want errTooLarge", err)
	}
}

func TestAnArchivePastTheFileLimitIsRefused(t *testing.T) {
	defer func(n int) { maxFiles = n }(maxFiles)
	maxFiles = 1
	_, _, err := readBundle(tarball(t, entry{name: "a.md", body: md("Doc", "")}, entry{name: "b.md", body: md("Doc", "")}), "docs")
	if !errors.Is(err, errTooLarge) {
		t.Fatalf("err = %v, want errTooLarge", err)
	}
}

func TestABodyThatIsNotGzipIsRefused(t *testing.T) {
	if _, _, err := readBundle(strings.NewReader("plain text"), "docs"); err == nil || errors.Is(err, errTooLarge) {
		t.Fatalf("err = %v, want a format error", err)
	}
}
