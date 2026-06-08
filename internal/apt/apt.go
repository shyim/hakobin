package apt

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

type RepositoryPath struct {
	Distribution string
	Component    string
	Architecture string
}

func (r RepositoryPath) PackagesPath() string {
	return fmt.Sprintf("dists/%s/%s/binary-%s/Packages", r.Distribution, r.Component, r.Architecture)
}

func (r RepositoryPath) PackagesGzPath() string {
	return r.PackagesPath() + ".gz"
}

func (r RepositoryPath) ReleasePath() string {
	return fmt.Sprintf("dists/%s/Release", r.Distribution)
}

func (r RepositoryPath) PoolPath(pkg string) string {
	var prefix string
	if strings.HasPrefix(pkg, "lib") && len(pkg) > 3 {
		prefix = pkg[:4]
	} else if len(pkg) > 0 {
		prefix = pkg[:1]
	}
	return fmt.Sprintf("pool/%s/%s/%s", r.Component, prefix, pkg)
}

type PackageEntry struct {
	Package       string
	Version       string
	Architecture  string
	Filename      string
	Size          uint64
	MD5sum        string
	SHA1          string
	SHA256        string
	Maintainer    string
	Description   string
	Section       string
	Priority      string
	InstalledSize uint64
	Depends       string
	Recommends    string
	Suggests      string
	Conflicts     string
	Breaks        string
	Replaces      string
	Provides      string
	Homepage      string
	Extra         map[string]string
}

func PackageEntryFromFields(fields map[string]string) (*PackageEntry, error) {
	pkg, err := required(fields, "Package")
	if err != nil {
		return nil, err
	}
	version, err := required(fields, "Version")
	if err != nil {
		return nil, err
	}
	arch, err := required(fields, "Architecture")
	if err != nil {
		return nil, err
	}
	filename, err := required(fields, "Filename")
	if err != nil {
		return nil, err
	}
	sizeStr, err := required(fields, "Size")
	if err != nil {
		return nil, err
	}
	size, err := strconv.ParseUint(sizeStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid Size field for %s: %w", pkg, err)
	}
	md5sum, err := required(fields, "MD5sum")
	if err != nil {
		return nil, err
	}
	sha1Val, err := required(fields, "SHA1")
	if err != nil {
		return nil, err
	}
	sha256Val, err := required(fields, "SHA256")
	if err != nil {
		return nil, err
	}

	known := map[string]bool{
		"Package":        true,
		"Version":        true,
		"Architecture":   true,
		"Filename":       true,
		"Size":           true,
		"MD5sum":         true,
		"SHA1":           true,
		"SHA256":         true,
		"Maintainer":     true,
		"Description":    true,
		"Section":        true,
		"Priority":       true,
		"Installed-Size": true,
		"Depends":        true,
		"Recommends":     true,
		"Suggests":       true,
		"Conflicts":      true,
		"Breaks":         true,
		"Replaces":       true,
		"Provides":       true,
		"Homepage":       true,
	}

	var instSize uint64
	if isStr, ok := fields["Installed-Size"]; ok {
		instSize, _ = strconv.ParseUint(isStr, 10, 64)
	}

	extra := make(map[string]string)
	for k, v := range fields {
		if !known[k] && v != "" {
			extra[k] = v
		}
	}

	return &PackageEntry{
		Package:       pkg,
		Version:       version,
		Architecture:  arch,
		Filename:      filename,
		Size:          size,
		MD5sum:        md5sum,
		SHA1:          sha1Val,
		SHA256:        sha256Val,
		Maintainer:    fields["Maintainer"],
		Description:   fields["Description"],
		Section:       fields["Section"],
		Priority:      fields["Priority"],
		InstalledSize: instSize,
		Depends:       fields["Depends"],
		Recommends:    fields["Recommends"],
		Suggests:      fields["Suggests"],
		Conflicts:     fields["Conflicts"],
		Breaks:        fields["Breaks"],
		Replaces:      fields["Replaces"],
		Provides:      fields["Provides"],
		Homepage:      fields["Homepage"],
		Extra:         extra,
	}, nil
}

type ReleaseFileEntry struct {
	Filename string
	Size     int
	MD5      string
	SHA1     string
	SHA256   string
}

type ReleaseFile struct {
	Origin        string
	Label         string
	Description   string
	Distribution  string
	Components    []string
	Architectures []string
	Date          time.Time
	Files         []ReleaseFileEntry
}

func (r *ReleaseFile) Generate() []byte {
	var out strings.Builder
	fmt.Fprintf(&out, "Origin: %s\n", defaulted(r.Origin, "APT S3 Repository"))
	fmt.Fprintf(&out, "Label: %s\n", defaulted(r.Label, "APT S3 Repository"))
	fmt.Fprintf(&out, "Suite: %s\n", r.Distribution)
	fmt.Fprintf(&out, "Codename: %s\n", r.Distribution)
	fmt.Fprintf(&out, "Date: %s\n", r.Date.UTC().Format("Mon, 02 Jan 2006 15:04:05 UTC"))
	fmt.Fprintf(&out, "Architectures: %s\n", strings.Join(r.Architectures, " "))
	fmt.Fprintf(&out, "Components: %s\n", strings.Join(r.Components, " "))
	fmt.Fprintf(&out, "Description: %s\n", defaulted(r.Description, "APT repository hosted on S3"))

	if len(r.Files) > 0 {
		out.WriteString("MD5Sum:\n")
		for _, f := range r.Files {
			fmt.Fprintf(&out, " %s %16d %s\n", f.MD5, f.Size, f.Filename)
		}
		out.WriteString("SHA1:\n")
		for _, f := range r.Files {
			fmt.Fprintf(&out, " %s %16d %s\n", f.SHA1, f.Size, f.Filename)
		}
		out.WriteString("SHA256:\n")
		for _, f := range r.Files {
			fmt.Fprintf(&out, " %s %16d %s\n", f.SHA256, f.Size, f.Filename)
		}
	}

	return []byte(out.String())
}

func ParseControlParagraphs(content string) ([]map[string]string, error) {
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

func ParsePackages(content string) ([]PackageEntry, error) {
	paras, err := ParseControlParagraphs(content)
	if err != nil {
		return nil, err
	}

	var entries []PackageEntry
	for _, fields := range paras {
		entry, err := PackageEntryFromFields(fields)
		if err != nil {
			return nil, err
		}
		entries = append(entries, *entry)
	}
	return entries, nil
}

func GeneratePackagesContent(packages []PackageEntry) string {
	if len(packages) == 0 {
		return ""
	}

	var entries []string
	for _, pkg := range packages {
		entries = append(entries, generatePackageEntry(&pkg))
	}
	return strings.Join(entries, "\n\n") + "\n\n"
}

func generatePackageEntry(pkg *PackageEntry) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("Package: %s", pkg.Package))
	lines = append(lines, fmt.Sprintf("Version: %s", pkg.Version))
	lines = append(lines, fmt.Sprintf("Architecture: %s", pkg.Architecture))

	pushOpt(&lines, "Maintainer", pkg.Maintainer)
	if pkg.InstalledSize > 0 {
		lines = append(lines, fmt.Sprintf("Installed-Size: %d", pkg.InstalledSize))
	}
	pushOpt(&lines, "Depends", pkg.Depends)
	pushOpt(&lines, "Recommends", pkg.Recommends)
	pushOpt(&lines, "Suggests", pkg.Suggests)
	pushOpt(&lines, "Conflicts", pkg.Conflicts)
	pushOpt(&lines, "Breaks", pkg.Breaks)
	pushOpt(&lines, "Replaces", pkg.Replaces)
	pushOpt(&lines, "Provides", pkg.Provides)
	pushOpt(&lines, "Section", pkg.Section)
	pushOpt(&lines, "Priority", pkg.Priority)
	pushOpt(&lines, "Homepage", pkg.Homepage)

	// Extra keys need to be sorted for deterministic behavior
	var extraKeys []string
	for k := range pkg.Extra {
		extraKeys = append(extraKeys, k)
	}
	sort.Strings(extraKeys)
	for _, k := range extraKeys {
		lines = append(lines, fmt.Sprintf("%s: %s", k, pkg.Extra[k]))
	}

	lines = append(lines, fmt.Sprintf("Filename: %s", pkg.Filename))
	lines = append(lines, fmt.Sprintf("Size: %d", pkg.Size))
	lines = append(lines, fmt.Sprintf("MD5sum: %s", pkg.MD5sum))
	lines = append(lines, fmt.Sprintf("SHA1: %s", pkg.SHA1))
	lines = append(lines, fmt.Sprintf("SHA256: %s", pkg.SHA256))
	pushOpt(&lines, "Description", pkg.Description)

	return strings.Join(lines, "\n")
}

func pushOpt(lines *[]string, key, value string) {
	if strings.TrimSpace(value) != "" {
		*lines = append(*lines, fmt.Sprintf("%s: %s", key, value))
	}
}

func required(fields map[string]string, key string) (string, error) {
	val, ok := fields[key]
	if !ok || strings.TrimSpace(val) == "" {
		return "", fmt.Errorf("package entry missing required '%s' field", key)
	}
	return val, nil
}

func defaulted(value, def string) string {
	if strings.TrimSpace(value) == "" {
		return def
	}
	return value
}

func CalculateHashes(data []byte) (string, string, string) {
	md5Sum := md5.Sum(data)
	sha1Sum := sha1.Sum(data)
	sha256Sum := sha256.Sum256(data)
	return hex.EncodeToString(md5Sum[:]), hex.EncodeToString(sha1Sum[:]), hex.EncodeToString(sha256Sum[:])
}

func CompressGzip(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, err := w.Write(data)
	if err != nil {
		return nil, err
	}
	w.Close()
	return buf.Bytes(), nil
}
