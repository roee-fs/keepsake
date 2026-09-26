package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/roee-fs/keepsake/internal/store"
	"github.com/roee-fs/keepsake/okf"
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

func TestBytesAfterTheArchiveAreRefused(t *testing.T) {
	body := append(tarball(t, entry{name: "a.md", body: md("Doc", "")}).Bytes(), "not a second gzip member"...)
	if _, _, err := readBundle(bytes.NewReader(body), "docs"); err == nil || errors.Is(err, errTooLarge) {
		t.Fatalf("err = %v, want a format error", err)
	}
}

func TestACorruptChecksumIsRefused(t *testing.T) {
	body := tarball(t, entry{name: "a.md", body: md("Doc", "")}).Bytes()
	// The gzip trailer is the CRC-32 then the length, so this flips a CRC byte.
	body[len(body)-8] ^= 0xff
	if _, _, err := readBundle(bytes.NewReader(body), "docs"); err == nil || errors.Is(err, errTooLarge) {
		t.Fatalf("err = %v, want a checksum error", err)
	}
}

func TestASparseEntryCannotInflatePastTheUnpackedLimit(t *testing.T) {
	defer func(n int64) { maxUnpacked = n }(maxUnpacked)
	maxUnpacked = 2048
	_, _, err := readBundle(sparseBundle(t, "sparse.md", 8192), "docs")
	if !errors.Is(err, errTooLarge) {
		t.Fatalf("err = %v, want errTooLarge", err)
	}
}

// sparseBundle hand-crafts a gzipped tar holding one PAX GNU-sparse entry: a header
// declaring logicalSize bytes of content backed by zero physical bytes (an empty
// sparse map, i.e. the whole file is one hole). archive/tar's Writer refuses to write
// GNU.sparse.* PAX records itself, so the header bytes are built by hand.
func sparseBundle(t *testing.T, name string, logicalSize int64) *bytes.Buffer {
	t.Helper()
	pax := paxRecord("GNU.sparse.major", "0") +
		paxRecord("GNU.sparse.minor", "1") +
		paxRecord("GNU.sparse.size", strconv.FormatInt(logicalSize, 10)) +
		paxRecord("GNU.sparse.numblocks", "0") +
		paxRecord("GNU.sparse.map", "")

	var raw bytes.Buffer
	raw.Write(rawTarHeader("PaxHeaders.0/"+name, tar.TypeXHeader, int64(len(pax))))
	raw.WriteString(pax)
	raw.Write(make([]byte, blockPadding(int64(len(pax)))))
	raw.Write(rawTarHeader(name, tar.TypeReg, 0))

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func blockPadding(n int64) int64 { return -n & 511 }

// paxRecord formats one PAX extended-header record: "<length> <key>=<value>\n",
// where length counts itself, as required by the POSIX pax format.
func paxRecord(k, v string) string {
	size := len(k) + len(v) + 3 // "=", "\n", and a first guess at the length digits
	size += len(strconv.Itoa(size))
	rec := strconv.Itoa(size) + " " + k + "=" + v + "\n"
	if len(rec) != size {
		size = len(rec)
		rec = strconv.Itoa(size) + " " + k + "=" + v + "\n"
	}
	return rec
}

// rawTarHeader builds one 512-byte USTAR header block. Only the fields readBundle's
// path needs are set; archive/tar fills the rest with sensible zero values.
func rawTarHeader(name string, typ byte, size int64) []byte {
	var b [512]byte
	copy(b[0:100], name)
	octalField(b[100:108], 0o644)
	octalField(b[108:116], 0)
	octalField(b[116:124], 0)
	octalField(b[124:136], size)
	octalField(b[136:148], 0)
	for i := 148; i < 156; i++ {
		b[i] = ' ' // checksum field reads as spaces while the checksum itself is computed
	}
	b[156] = typ
	copy(b[257:263], "ustar\x00")
	copy(b[263:265], "00")

	var sum int64
	for _, c := range b {
		sum += int64(c)
	}
	copy(b[148:154], fmt.Sprintf("%06o", sum))
	b[154], b[155] = 0, ' '
	return b[:]
}

func octalField(field []byte, n int64) {
	copy(field, fmt.Sprintf("%0*o", len(field)-1, n))
	field[len(field)-1] = 0
}

func put(t *testing.T, h http.Handler, prefix string, body io.Reader, tenant uuid.UUID) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/bundle?prefix="+prefix, body)
	req.Header.Set("Authorization", "Bearer "+issuer.Mint(tenant, "platform-ingest", time.Minute))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func listPaths(t *testing.T, cs *store.ConceptStore, tenant uuid.UUID) []string {
	t.Helper()
	got, err := cs.List(ctx, tenant, "")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range got {
		out = append(out, p.Path)
	}
	return out
}

func TestAnUploadReplacesOnlyTheCallersPrefix(t *testing.T) {
	cs := conceptStore(t)
	h := issuer.Middleware(replaceBundle(cs))
	mine, theirs := uuid.New(), uuid.New()
	for _, tenant := range []uuid.UUID{mine, theirs} {
		if _, err := cs.ImportMany(ctx, tenant, []okf.Concept{{Path: "docs/old", Type: "Doc"}, {Path: "notes/n", Type: "Note"}}, "agent"); err != nil {
			t.Fatal(err)
		}
	}
	code, body := put(t, h, "docs", tarball(t, entry{name: "a.md", body: md("Doc", "a")}), mine)
	var got map[string]int
	if code != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil || got["written"] != 1 || got["deleted"] != 1 {
		t.Fatalf("PUT = %d %s", code, body)
	}
	if p := listPaths(t, cs, mine); !reflect.DeepEqual(p, []string{"docs/a", "notes/n"}) {
		t.Fatalf("mine = %v", p)
	}
	if p := listPaths(t, cs, theirs); !reflect.DeepEqual(p, []string{"docs/old", "notes/n"}) {
		t.Fatalf("theirs = %v", p)
	}
	revs, err := cs.Revisions(ctx, mine, 10)
	if err != nil || revs[len(revs)-1].UpdatedBy != "platform-ingest" {
		t.Fatalf("revisions = %+v, %v, want the token's subject as actor", revs, err)
	}
}

func TestAnUploadWithAProblemWritesNothing(t *testing.T) {
	cs := conceptStore(t)
	tenant := uuid.New()
	if _, err := cs.ImportMany(ctx, tenant, []okf.Concept{{Path: "docs/old", Type: "Doc"}}, "agent"); err != nil {
		t.Fatal(err)
	}
	code, body := put(t, issuer.Middleware(replaceBundle(cs)), "docs",
		tarball(t, entry{name: "a.md", body: md("Doc", "a")}, entry{name: "b.md", body: "no type"}), tenant)
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "b.md: type is required") {
		t.Fatalf("PUT = %d %s", code, body)
	}
	if p := listPaths(t, cs, tenant); !reflect.DeepEqual(p, []string{"docs/old"}) {
		t.Fatalf("paths = %v", p)
	}
}

func TestAnEmptyUploadIsRefused(t *testing.T) {
	cs := conceptStore(t)
	tenant := uuid.New()
	if _, err := cs.ImportMany(ctx, tenant, []okf.Concept{{Path: "docs/old", Type: "Doc"}}, "agent"); err != nil {
		t.Fatal(err)
	}
	code, body := put(t, issuer.Middleware(replaceBundle(cs)), "docs", tarball(t, entry{name: "README.txt", body: "x"}), tenant)
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "bundle holds no concepts") {
		t.Fatalf("PUT = %d %s", code, body)
	}
	if p := listPaths(t, cs, tenant); !reflect.DeepEqual(p, []string{"docs/old"}) {
		t.Fatalf("paths = %v", p)
	}
}

func TestAnUploadNeedsAValidPrefix(t *testing.T) {
	h := issuer.Middleware(replaceBundle(conceptStore(t)))
	for _, prefix := range []string{"", ".", "/docs", "docs/", "../x", "a//b", "a%00b"} {
		if code, _ := put(t, h, prefix, tarball(t, entry{name: "a.md", body: md("Doc", "")}), uuid.New()); code != http.StatusUnprocessableEntity {
			t.Errorf("prefix %q = %d, want 422", prefix, code)
		}
	}
}

func TestAnOversizedUploadIsRefused(t *testing.T) {
	defer func(n int64) { maxUpload = n }(maxUpload)
	// Under any gzipped tar, however well it compresses.
	maxUpload = 16
	code, _ := put(t, issuer.Middleware(replaceBundle(conceptStore(t))), "docs",
		tarball(t, entry{name: "a.md", body: md("Doc", strings.Repeat("unique words ", 200))}), uuid.New())
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("PUT = %d, want 413", code)
	}
}

func TestAnUploadDuringAnotherIsRefused(t *testing.T) {
	uploads <- struct{}{}
	defer func() { <-uploads }()
	req := httptest.NewRequest(http.MethodPut, "/bundle?prefix=docs", tarball(t, entry{name: "a.md", body: md("Doc", "a")}))
	req.Header.Set("Authorization", "Bearer "+issuer.Mint(uuid.New(), "platform-ingest", time.Minute))
	rec := httptest.NewRecorder()
	issuer.Middleware(replaceBundle(conceptStore(t))).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "5" {
		t.Fatalf("PUT = %d, Retry-After %q, want 503, 5", rec.Code, rec.Header().Get("Retry-After"))
	}
}
