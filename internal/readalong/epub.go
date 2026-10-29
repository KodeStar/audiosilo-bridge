package readalong

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/kodestar/audiosilo-bridge/internal/epubcfi"
)

// MaxEbookBytes bounds the EPUB the Bridge reads (decision 13).
const MaxEbookBytes = 256 << 20

// maxEntryBytes bounds one decompressed member (a content document, the OPF).
const maxEntryBytes = 64 << 20

// The ways an ebook can't be aligned to, in words an admin reads.
var (
	ErrDRM         = errors.New("the book's ebook is DRM-protected, so its text can't be read")
	ErrNotEPUB     = errors.New("the book's ebook isn't a readable EPUB")
	ErrEbookTooBig = errors.New("the book's ebook is over 256 MiB")
)

// Section is one spine item of an EPUB.
type Section struct {
	Index int
	IDRef string
	// Href is the member's path in the zip; RelHref the same relative to the
	// OPF's folder (the map's href).
	Href      string
	RelHref   string
	MediaType string
	Linear    bool
	// CFI is the section's own CFI (foliate's fromElements over the itemrefs).
	CFI string
}

// EPUB is an opened EPUB: its spine and members, the file's size and sha256.
type EPUB struct {
	OPFPath string
	Spine   []Section
	Size    int64
	SHA256  string
	members map[string]*zip.File
}

// OpenEPUB reads the EPUB at path (a regular file of at most MaxEbookBytes),
// refuses DRM, and parses its container and OPF.
func OpenEPUB(p string) (*EPUB, error) {
	f, err := os.Open(p) //nolint:gosec // the caller resolved it through the checked mapping
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrNotEPUB
	}
	if info.Size() > MaxEbookBytes {
		return nil, ErrEbookTooBig
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxEbookBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxEbookBytes {
		return nil, ErrEbookTooBig
	}
	return ReadEPUB(raw)
}

// ReadEPUB is OpenEPUB over the file's bytes.
func ReadEPUB(raw []byte) (*EPUB, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, ErrNotEPUB
	}
	sum := sha256.Sum256(raw)
	e := &EPUB{Size: int64(len(raw)), SHA256: hex.EncodeToString(sum[:]), members: map[string]*zip.File{}}
	for _, m := range zr.File {
		e.members[m.Name] = m
	}
	if _, ok := e.members["META-INF/rights.xml"]; ok {
		return nil, ErrDRM
	}
	if enc, err := e.Read("META-INF/encryption.xml"); err == nil && HasDRM(enc) {
		return nil, ErrDRM
	}
	if err := e.parsePackage(); err != nil {
		return nil, err
	}
	return e, nil
}

// obfuscation are the font obfuscation algorithms, which aren't DRM.
var obfuscation = map[string]bool{
	"http://www.idpf.org/2008/embedding": true,
	"http://ns.adobe.com/pdf/enc#RC":     true,
}

var encryptionMethod = regexp.MustCompile(`<(?:[\w-]+:)?EncryptionMethod\b[^>]*\bAlgorithm\s*=\s*["']([^"']+)["']`)

// HasDRM reports whether encryption.xml encrypts anything with more than font
// obfuscation (the reader's hasDrm rule).
func HasDRM(encryptionXML []byte) bool {
	for _, m := range encryptionMethod.FindAllSubmatch(encryptionXML, -1) {
		if !obfuscation[string(m[1])] {
			return true
		}
	}
	return false
}

// errNoMember is a member the EPUB doesn't have.
var errNoMember = errors.New("no such member")

// Read reads one member (bounded).
func (e *EPUB) Read(name string) ([]byte, error) {
	m, ok := e.members[name]
	if !ok {
		return nil, errNoMember
	}
	rc, err := m.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, maxEntryBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxEntryBytes {
		return nil, fmt.Errorf("%s: over %d bytes", name, maxEntryBytes)
	}
	return b, nil
}

func (e *EPUB) parsePackage() error {
	raw, err := e.Read("META-INF/container.xml")
	if err != nil {
		return ErrNotEPUB
	}
	container, err := epubcfi.ParseXML(raw)
	if err != nil {
		return ErrNotEPUB
	}
	roots := container.ElementsByName("rootfile")
	if len(roots) == 0 || roots[0].Attr("full-path") == "" {
		return ErrNotEPUB
	}
	e.OPFPath = roots[0].Attr("full-path")
	raw, err = e.Read(e.OPFPath)
	if err != nil {
		return ErrNotEPUB
	}
	opf, err := epubcfi.ParseXML(raw)
	if err != nil {
		return ErrNotEPUB
	}
	base := path.Dir(e.OPFPath)
	type item struct{ href, mediaType string }
	manifest := map[string]item{}
	for _, it := range opf.ElementsByName("item") {
		href := it.Attr("href")
		if dec, err := url.PathUnescape(href); err == nil {
			href = dec
		}
		manifest[it.Attr("id")] = item{href: joinPosix(base, href), mediaType: it.Attr("media-type")}
	}
	refs := opf.ElementsByName("itemref")
	cfis := epubcfi.FromElements(refs)
	if len(refs) == 0 || len(cfis) != len(refs) {
		return ErrNotEPUB
	}
	for i, ir := range refs {
		m, ok := manifest[ir.Attr("idref")]
		if !ok {
			continue
		}
		e.Spine = append(e.Spine, Section{Index: i, IDRef: ir.Attr("idref"), Href: m.href, RelHref: relPosix(base, m.href),
			MediaType: m.mediaType, Linear: ir.Attr("linear") != "no", CFI: cfis[i]})
	}
	return nil
}

// joinPosix is path.posix.join(base, href) with "." as no base.
func joinPosix(base, href string) string {
	if base == "." || base == "" {
		return path.Clean(href)
	}
	return path.Join(base, href)
}

// relPosix is path.posix.relative(base, target) for two cleaned paths.
func relPosix(base, target string) string {
	if base == "." || base == "" {
		return target
	}
	b, t := strings.Split(base, "/"), strings.Split(target, "/")
	i := 0
	for i < len(b) && i < len(t) && b[i] == t[i] {
		i++
	}
	parts := make([]string, 0, len(b)-i+len(t)-i)
	for range b[i:] {
		parts = append(parts, "..")
	}
	return strings.Join(append(parts, t[i:]...), "/")
}

// IsContent reports whether a section is a content document (XHTML or HTML).
func (s Section) IsContent() bool { return strings.Contains(s.MediaType, "html") }

// Document parses a section's content document by its media type.
func (e *EPUB) Document(s Section) (*epubcfi.Document, error) {
	raw, err := e.Read(s.Href)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.Href, err)
	}
	doc, err := epubcfi.ParseDocument(raw, s.MediaType)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.Href, err)
	}
	return doc, nil
}
