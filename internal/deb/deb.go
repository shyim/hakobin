package deb

import (
	"archive/tar"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

type DebPackage struct {
	Filename string
	Size     uint64
	MD5      string
	SHA1     string
	SHA256   string
	Control  map[string]string
	Raw      []byte
}

func FromPath(path string) (*DebPackage, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}
	filename := filepath.Base(path)
	return Parse(filename, raw)
}

func Parse(filename string, raw []byte) (*DebPackage, error) {
	size := uint64(len(raw))

	md5Sum := md5.Sum(raw)
	md5Str := hex.EncodeToString(md5Sum[:])

	sha1Sum := sha1.Sum(raw)
	sha1Str := hex.EncodeToString(sha1Sum[:])

	sha256Sum := sha256.Sum256(raw)
	sha256Str := hex.EncodeToString(sha256Sum[:])

	control, err := extractControl(raw)
	if err != nil {
		return nil, err
	}

	return &DebPackage{
		Filename: filename,
		Size:     size,
		MD5:      md5Str,
		SHA1:     sha1Str,
		SHA256:   sha256Str,
		Control:  control,
		Raw:      raw,
	}, nil
}

// packageNameRe matches valid Debian package names: at least two characters,
// starting alphanumeric, then lowercase letters, digits, '+', '-', '.'.
var packageNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)

// versionRe matches the characters permitted in a Debian version string
// (epoch, upstream version, and revision). It deliberately excludes path
// separators and anything that could escape a pool path.
var versionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+.~:-]*$`)

// archRe matches Debian architecture names such as amd64, arm64, all, i386.
var archRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func (d *DebPackage) Package() (string, error) {
	val, ok := d.Control["Package"]
	if !ok {
		return "", fmt.Errorf("package does not specify Package")
	}
	if !packageNameRe.MatchString(val) {
		return "", fmt.Errorf("invalid Package name %q", val)
	}
	return val, nil
}

func (d *DebPackage) Version() (string, error) {
	val, ok := d.Control["Version"]
	if !ok {
		return "", fmt.Errorf("package does not specify Version")
	}
	if !versionRe.MatchString(val) {
		return "", fmt.Errorf("invalid Version %q", val)
	}
	return val, nil
}

func (d *DebPackage) Architecture() (string, error) {
	val, ok := d.Control["Architecture"]
	if !ok {
		return "", fmt.Errorf("package does not specify Architecture")
	}
	if !archRe.MatchString(val) {
		return "", fmt.Errorf("invalid Architecture %q", val)
	}
	return val, nil
}

func (d *DebPackage) GeneratedFilename() (string, error) {
	pkg, err := d.Package()
	if err != nil {
		return "", err
	}
	ver, err := d.Version()
	if err != nil {
		return "", err
	}
	arch, err := d.Architecture()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s_%s_%s.deb", pkg, ver, arch), nil
}

// reservedEntryFields are computed by the repository from the artifact itself.
// A control field in the .deb must never be able to override them, or a crafted
// package could point apt at an arbitrary file or forge a checksum.
var reservedEntryFields = map[string]bool{
	"Filename": true,
	"Size":     true,
	"MD5sum":   true,
	"SHA1":     true,
	"SHA256":   true,
}

func (d *DebPackage) PackageEntry(poolPath string) string {
	var keys []string
	for k := range d.Control {
		if reservedEntryFields[k] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&out, "%s: %s\n", k, foldFieldValue(d.Control[k]))
	}
	fmt.Fprintf(&out, "Filename: %s\n", poolPath)
	fmt.Fprintf(&out, "Size: %d\n", d.Size)
	fmt.Fprintf(&out, "MD5sum: %s\n", d.MD5)
	fmt.Fprintf(&out, "SHA1: %s\n", d.SHA1)
	fmt.Fprintf(&out, "SHA256: %s\n", d.SHA256)
	return out.String()
}

// foldFieldValue neutralizes control-field injection: every embedded newline in
// a field value must be followed by a space or tab (RFC822 continuation) so the
// value cannot forge a new field or an entire package stanza in the Packages
// index. A bare newline (or a paragraph break) from a crafted .deb is re-folded
// into a continuation line.
func foldFieldValue(value string) string {
	if !strings.ContainsAny(value, "\n\r") {
		return value
	}
	normalized := strings.ReplaceAll(value, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	lines := strings.Split(normalized, "\n")
	var out strings.Builder
	for i, line := range lines {
		if i > 0 {
			out.WriteString("\n")
			// Ensure the continuation is a valid folded line and never blank
			// (a blank line would terminate the stanza).
			if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
				out.WriteString(" ")
			}
			if strings.TrimSpace(line) == "" {
				out.WriteString(".")
			}
		}
		out.WriteString(line)
	}
	return out.String()
}

func extractControl(data []byte) (map[string]string, error) {
	entries, err := parseAr(data)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name, "control.tar") {
			return extractControlFromTar(entry.Data, entry.Name)
		}
	}
	return nil, fmt.Errorf("control.tar not found in deb package")
}

func extractControlFromTar(data []byte, name string) (map[string]string, error) {
	tarData, err := decompressControlTar(data, name)
	if err != nil {
		return nil, err
	}

	tr := tar.NewReader(bytes.NewReader(tarData))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		normalizedPath := filepath.Clean(hdr.Name)
		if normalizedPath == "control" || normalizedPath == "./control" || normalizedPath == ".control" {
			controlData, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			paragraphs, err := parseControlParagraphs(string(controlData))
			if err != nil {
				return nil, err
			}
			if len(paragraphs) == 0 {
				return nil, fmt.Errorf("control file did not contain a paragraph")
			}
			return paragraphs[0], nil
		}
	}

	return nil, fmt.Errorf("control file not found in %s", name)
}

func decompressControlTar(data []byte, name string) ([]byte, error) {
	var r io.Reader = bytes.NewReader(data)
	var err error

	if strings.HasSuffix(name, ".gz") || bytes.HasPrefix(data, []byte{0x1f, 0x8b}) {
		r, err = gzip.NewReader(r)
		if err != nil {
			return nil, err
		}
	} else if strings.HasSuffix(name, ".xz") || bytes.HasPrefix(data, []byte{0xfd, '7', 'z', 'X', 'Z'}) {
		r, err = xz.NewReader(r)
		if err != nil {
			return nil, err
		}
	} else if strings.HasSuffix(name, ".zst") || bytes.HasPrefix(data, []byte{0x28, 0xb5, 0x2f, 0xfd}) {
		var decoder *zstd.Decoder
		decoder, err = zstd.NewReader(r)
		if err != nil {
			return nil, err
		}
		defer decoder.Close()
		r = decoder
	} else if strings.HasSuffix(name, ".bz2") || bytes.HasPrefix(data, []byte("BZh")) {
		r = bzip2.NewReader(r)
	}

	return io.ReadAll(r)
}

type arEntry struct {
	Name string
	Data []byte
}

func parseAr(data []byte) ([]arEntry, error) {
	if len(data) < 8 || string(data[:8]) != "!<arch>\n" {
		return nil, fmt.Errorf("not an ar archive")
	}

	var entries []arEntry
	pos := 8
	for pos < len(data) {
		if pos+60 > len(data) {
			return nil, fmt.Errorf("truncated ar header")
		}
		header := data[pos : pos+60]
		pos += 60

		if string(header[58:60]) != "`\n" {
			return nil, fmt.Errorf("invalid ar header ending")
		}

		name := strings.TrimSpace(string(header[0:16]))
		name = strings.TrimSuffix(name, "/")

		sizeStr := strings.TrimSpace(string(header[48:58]))
		var size int
		_, err := fmt.Sscanf(sizeStr, "%d", &size)
		if err != nil {
			return nil, fmt.Errorf("invalid ar member size: %w", err)
		}

		if pos+size > len(data) {
			return nil, fmt.Errorf("truncated ar member %s", name)
		}

		entryData := data[pos : pos+size]
		pos += size
		if size%2 == 1 && pos < len(data) {
			pos++
		}

		entries = append(entries, arEntry{
			Name: name,
			Data: entryData,
		})
	}
	return entries, nil
}

func parseControlParagraphs(content string) ([]map[string]string, error) {
	if strings.TrimSpace(content) == "" {
		return nil, nil
	}

	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	paragraphs := strings.Split(normalized, "\n\n")

	var result []map[string]string
	for _, p := range paragraphs {
		if strings.TrimSpace(p) == "" {
			continue
		}
		fields, err := parseParagraph(p)
		if err != nil {
			return nil, err
		}
		result = append(result, fields)
	}
	return result, nil
}

func parseParagraph(paragraph string) (map[string]string, error) {
	fields := make(map[string]string)
	var currentKey string

	lines := strings.Split(paragraph, "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			// Skip empty lines in the middle
			continue
		}

		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if currentKey == "" {
				return nil, fmt.Errorf("control continuation line without a preceding field")
			}
			fields[currentKey] = fields[currentKey] + "\n" + line
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid control field line: %s", line)
		}

		key := strings.TrimSpace(parts[0])
		if key == "" {
			return nil, fmt.Errorf("empty control field name")
		}
		value := strings.TrimLeft(parts[1], " \t")
		fields[key] = value
		currentKey = key
	}

	return fields, nil
}
