package rpm

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sassoftware/go-rpmutils"

	"github.com/shyim/hakobin/internal/openpgp"
)

const (
	RpmPrefix        = "rpm"
	RpmPublicKeyName = "RPM-GPG-KEY-hakobin.asc"

	// rpmFileGhost is RPMFILE_GHOST from rpm's rpmfileAttrs: a file that the
	// package owns but does not itself install (e.g. a log created at runtime).
	// Ghost state lives in the RPM file flags, not the unix file mode.
	rpmFileGhost = 1 << 6
)

type RpmPackage struct {
	Name          string          `json:"name"`
	Epoch         string          `json:"epoch"`
	Version       string          `json:"version"`
	Release       string          `json:"release"`
	Arch          string          `json:"arch"`
	Summary       string          `json:"summary"`
	Description   string          `json:"description"`
	License       string          `json:"license"`
	Vendor        string          `json:"vendor"`
	Group         string          `json:"group"`
	URL           string          `json:"url"`
	SourceRpm     string          `json:"source_rpm"`
	BuildTime     uint64          `json:"build_time"`
	FileTime      uint64          `json:"file_time"`
	HeaderStart   int             `json:"header_start"`
	HeaderEnd     int             `json:"header_end"`
	PackageSize   uint64          `json:"package_size"`
	InstalledSize uint64          `json:"installed_size"`
	ArchiveSize   uint64          `json:"archive_size"`
	Checksum      string          `json:"checksum"`
	Location      string          `json:"location"`
	Files         []RpmFile       `json:"files"`
	Provides      []RpmDependency `json:"provides"`
	Requires      []RpmDependency `json:"requires"`
	Conflicts     []RpmDependency `json:"conflicts"`
	Obsoletes     []RpmDependency `json:"obsoletes"`
	Changelogs    []RpmChangelog  `json:"changelogs"`
	Raw           []byte          `json:"raw"`
}

type RpmFile struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // "file", "dir", or "ghost"
}

type RpmDependency struct {
	Name    string `json:"name"`
	Flags   string `json:"flags"`
	Epoch   string `json:"epoch"`
	Version string `json:"version"`
	Release string `json:"release"`
}

type RpmChangelog struct {
	Author string `json:"author"`
	Date   uint64 `json:"date"`
	Text   string `json:"text"`
}

type MetadataFile struct {
	Kind         string
	Filename     string
	Data         []byte
	OpenChecksum string
	Checksum     string
	OpenSize     int
	Size         int
}

type RpmRepositoryId struct {
	Repo string
	Arch string
}

func FromPath(path string) (*RpmPackage, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}
	filename := filepath.Base(path)
	return FromBytes("Packages/"+filename, raw)
}

func FromBytes(location string, raw []byte) (*RpmPackage, error) {
	rpm, err := rpmutils.ReadRpm(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("failed to parse RPM package: %w", err)
	}

	nevra, err := rpm.Header.GetNEVRA()
	if err != nil {
		return nil, fmt.Errorf("failed to get NEVRA: %w", err)
	}

	summary, _ := rpm.Header.GetString(rpmutils.SUMMARY)
	description, _ := rpm.Header.GetString(rpmutils.DESCRIPTION)
	license, _ := rpm.Header.GetString(rpmutils.LICENSE)
	vendor, _ := rpm.Header.GetString(rpmutils.VENDOR)
	group, _ := rpm.Header.GetString(rpmutils.GROUP)
	url, _ := rpm.Header.GetString(rpmutils.URL)
	sourceRpm, _ := rpm.Header.GetString(rpmutils.SOURCERPM)

	buildTime := getIntFallback(rpm.Header, rpmutils.BUILDTIME)
	archiveSize := getIntFallback(rpm.Header, rpmutils.ARCHIVESIZE)
	if archiveSize == 0 {
		archiveSize = int64(len(raw))
	}

	headerRange := rpm.Header.GetRange()

	installedSize, _ := rpm.Header.InstalledSize()

	sum := sha256.Sum256(raw)
	checksum := hex.EncodeToString(sum[:])

	var files []RpmFile
	rpmFiles, err := rpm.Header.GetFiles()
	if err == nil {
		for _, f := range rpmFiles {
			kind := "file"
			// Ghost state comes from the RPM file flags, not the unix mode. A
			// symlink (mode S_IFLNK) is still a regular listed file for
			// repodata purposes; only RPMFILE_GHOST marks a ghost.
			if f.Flags()&rpmFileGhost != 0 {
				kind = "ghost"
			} else if f.Mode()&0170000 == 0040000 {
				kind = "dir"
			}
			files = append(files, RpmFile{
				Path: f.Name(),
				Kind: kind,
			})
		}
	}

	provides, _ := readDependencies(rpm.Header, rpmutils.PROVIDENAME, rpmutils.PROVIDEVERSION, rpmutils.PROVIDEFLAGS)
	requires, _ := readDependencies(rpm.Header, rpmutils.REQUIRENAME, rpmutils.REQUIREVERSION, rpmutils.REQUIREFLAGS)
	conflicts, _ := readDependencies(rpm.Header, rpmutils.CONFLICTNAME, rpmutils.CONFLICTVERSION, rpmutils.CONFLICTFLAGS)
	obsoletes, _ := readDependencies(rpm.Header, rpmutils.OBSOLETENAME, rpmutils.OBSOLETEVERSION, 1114) // 1114 is OBSOLETEFLAGS

	changelogs, _ := readChangelogs(rpm.Header)

	return &RpmPackage{
		Name:          nevra.Name,
		Epoch:         nevra.Epoch,
		Version:       nevra.Version,
		Release:       nevra.Release,
		Arch:          nevra.Arch,
		Summary:       summary,
		Description:   description,
		License:       license,
		Vendor:        vendor,
		Group:         group,
		URL:           url,
		SourceRpm:     sourceRpm,
		BuildTime:     uint64(buildTime),
		FileTime:      uint64(buildTime),
		HeaderStart:   headerRange.Start,
		HeaderEnd:     headerRange.End,
		PackageSize:   uint64(len(raw)),
		InstalledSize: uint64(installedSize),
		ArchiveSize:   uint64(archiveSize),
		Checksum:      checksum,
		Location:      location,
		Files:         files,
		Provides:      provides,
		Requires:      requires,
		Conflicts:     conflicts,
		Obsoletes:     obsoletes,
		Changelogs:    changelogs,
		Raw:           raw,
	}, nil
}

func (p *RpmPackage) Filename() string {
	return strings.TrimPrefix(p.Location, "Packages/")
}

func (p *RpmPackage) Signed(signingKeys *openpgp.SigningKeys) (*RpmPackage, error) {
	signedRaw, err := SignRpmPackage(p.Raw, signingKeys)
	if err != nil {
		return nil, err
	}
	return FromBytes(p.Location, signedRaw)
}

func SignRpmPackage(raw []byte, signingKeys *openpgp.SigningKeys) ([]byte, error) {
	if signingKeys == nil || signingKeys.Active == nil {
		return raw, nil
	}

	tempIn, err := os.CreateTemp("", "rpm-in-*.rpm")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tempIn.Name())
	defer tempIn.Close()

	if _, err := tempIn.Write(raw); err != nil {
		return nil, err
	}

	if _, err := tempIn.Seek(0, 0); err != nil {
		return nil, err
	}

	tempOutFile, err := os.CreateTemp("", "rpm-out-*.rpm")
	if err != nil {
		return nil, err
	}
	tempOutPath := tempOutFile.Name()
	tempOutFile.Close()
	defer os.Remove(tempOutPath)

	privateKey := signingKeys.Active.Entity.PrivateKey

	_, err = rpmutils.SignRpmFile(tempIn, tempOutPath, privateKey, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to sign RPM file: %w", err)
	}

	signedRaw, err := os.ReadFile(tempOutPath)
	if err != nil {
		return nil, err
	}

	return signedRaw, nil
}

func getIntFallback(hdr *rpmutils.RpmHeader, tag int) int64 {
	val, err := hdr.GetInt(tag)
	if err == nil {
		return int64(val)
	}
	vals, err := hdr.GetUint64s(tag)
	if err == nil && len(vals) > 0 {
		return int64(vals[0])
	}
	vals32, err := hdr.GetUint32s(tag)
	if err == nil && len(vals32) > 0 {
		return int64(vals32[0])
	}
	return 0
}

func readDependencies(hdr *rpmutils.RpmHeader, nameTag, versionTag, flagsTag int) ([]RpmDependency, error) {
	names, err := hdr.GetStrings(nameTag)
	if err != nil {
		if _, ok := err.(rpmutils.NoSuchTagError); ok {
			return nil, nil
		}
		return nil, err
	}
	versions, err := hdr.GetStrings(versionTag)
	if err != nil {
		versions = make([]string, len(names))
	}
	flags, err := hdr.GetUint64s(flagsTag)
	if err != nil {
		flags32, err2 := hdr.GetUint32s(flagsTag)
		if err2 == nil {
			flags = make([]uint64, len(flags32))
			for i, v := range flags32 {
				flags[i] = uint64(v)
			}
		} else {
			flags = make([]uint64, len(names))
		}
	}

	var deps []RpmDependency
	for i := 0; i < len(names); i++ {
		name := names[i]
		var version, flagStr string
		var epoch, rel string

		if i < len(versions) {
			version = versions[i]
			epoch, version, rel = splitEVR(version)
		}

		var flag uint64
		if i < len(flags) {
			flag = flags[i]
		}
		flagStr = formatFlags(flag)

		deps = append(deps, RpmDependency{
			Name:    name,
			Flags:   flagStr,
			Epoch:   epoch,
			Version: version,
			Release: rel,
		})
	}
	return deps, nil
}

func splitEVR(value string) (string, string, string) {
	epoch := "0"
	if idx := strings.Index(value, ":"); idx != -1 {
		epoch = value[:idx]
		value = value[idx+1:]
	}
	version := value
	release := ""
	if idx := strings.LastIndex(value, "-"); idx != -1 {
		version = value[:idx]
		release = value[idx+1:]
	}
	return epoch, version, release
}

// formatFlags maps the RPM dependency sense bits to the symbolic flag names
// that createrepo/repodata use (LT/GT/EQ/LE/GE). YUM/DNF-compatible clients
// expect these names in rpm:entry flags, not the operator glyphs.
func formatFlags(flags uint64) string {
	sense := flags & 0x0e
	switch sense {
	case 0x02:
		return "LT"
	case 0x04:
		return "GT"
	case 0x08:
		return "EQ"
	case 0x0a:
		return "LE"
	case 0x0c:
		return "GE"
	}
	return ""
}

func readChangelogs(hdr *rpmutils.RpmHeader) ([]RpmChangelog, error) {
	names, err := hdr.GetStrings(rpmutils.CHANGELOGNAME)
	if err != nil {
		if _, ok := err.(rpmutils.NoSuchTagError); ok {
			return nil, nil
		}
		return nil, err
	}
	times, _ := hdr.GetUint64s(rpmutils.CHANGELOGTIME)
	if times == nil {
		times32, _ := hdr.GetUint32s(rpmutils.CHANGELOGTIME)
		if times32 != nil {
			times = make([]uint64, len(times32))
			for i, v := range times32 {
				times[i] = uint64(v)
			}
		}
	}
	texts, _ := hdr.GetStrings(rpmutils.CHANGELOGTEXT)

	var changelogs []RpmChangelog
	for i := 0; i < len(names); i++ {
		var t uint64
		if times != nil && i < len(times) {
			t = times[i]
		}
		var txt string
		if texts != nil && i < len(texts) {
			txt = texts[i]
		}
		changelogs = append(changelogs, RpmChangelog{
			Author: names[i],
			Date:   t,
			Text:   txt,
		})
	}
	return changelogs, nil
}

func RpmRepositoryFromKey(key string) *RpmRepositoryId {
	if !strings.HasPrefix(key, RpmPrefix+"/") {
		return nil
	}
	path := strings.TrimPrefix(key, RpmPrefix+"/")
	parts := strings.Split(path, "/")
	if len(parts) < 3 {
		return nil
	}
	repo := parts[0]
	arch := parts[1]
	marker := parts[2]

	if repo == "" || arch == "" {
		return nil
	}

	if marker == "Packages" || marker == "repodata" {
		return &RpmRepositoryId{
			Repo: repo,
			Arch: arch,
		}
	}
	return nil
}

type RepositoryMetadata struct {
	Primary   MetadataFile
	Filelists MetadataFile
	Other     MetadataFile
	Repomd    string
}

func GenerateRepositoryMetadata(packages []RpmPackage) (*RepositoryMetadata, error) {
	primaryXML := generatePrimaryXML(packages)
	filelistsXML := generateFilelistsXML(packages)
	otherXML := generateOtherXML(packages)

	primary, err := makeMetadataFile("primary", []byte(primaryXML))
	if err != nil {
		return nil, err
	}
	filelists, err := makeMetadataFile("filelists", []byte(filelistsXML))
	if err != nil {
		return nil, err
	}
	other, err := makeMetadataFile("other", []byte(otherXML))
	if err != nil {
		return nil, err
	}

	repomd := generateRepomdXML(primary, filelists, other)

	return &RepositoryMetadata{
		Primary:   *primary,
		Filelists: *filelists,
		Other:     *other,
		Repomd:    repomd,
	}, nil
}

func SignRpmMetadata(repomd []byte, signingKeys *openpgp.SigningKeys) ([]byte, []byte, error) {
	if signingKeys == nil || signingKeys.Active == nil {
		return nil, nil, nil
	}
	sig, err := signingKeys.Active.SignDetached(repomd)
	if err != nil {
		return nil, nil, err
	}
	pubKey, err := signingKeys.PublicKeyArmored()
	if err != nil {
		return nil, nil, err
	}
	return []byte(sig), pubKey, nil
}

func makeMetadataFile(kind string, xmlData []byte) (*MetadataFile, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(xmlData); err != nil {
		return nil, err
	}
	w.Close()

	compressedData := buf.Bytes()
	openSum := sha256.Sum256(xmlData)
	closedSum := sha256.Sum256(compressedData)

	openChecksum := hex.EncodeToString(openSum[:])
	closedChecksum := hex.EncodeToString(closedSum[:])

	filename := fmt.Sprintf("%s-%s.xml.gz", closedChecksum, kind)

	return &MetadataFile{
		Kind:         kind,
		Filename:     filename,
		Data:         compressedData,
		OpenChecksum: openChecksum,
		Checksum:     closedChecksum,
		OpenSize:     len(xmlData),
		Size:         len(compressedData),
	}, nil
}

func generatePrimaryXML(packages []RpmPackage) string {
	var out strings.Builder
	out.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	fmt.Fprintf(&out, "<metadata xmlns=\"http://linux.duke.edu/metadata/common\" xmlns:rpm=\"http://linux.duke.edu/metadata/rpm\" packages=\"%d\">\n", len(packages))

	for _, p := range packages {
		out.WriteString("<package type=\"rpm\">")
		tag(&out, "name", p.Name)
		tag(&out, "arch", p.Arch)
		fmt.Fprintf(&out, "<version epoch=\"%s\" ver=\"%s\" rel=\"%s\"/>", xmlEscape(p.Epoch), xmlEscape(p.Version), xmlEscape(p.Release))
		fmt.Fprintf(&out, "<checksum type=\"sha256\" pkgid=\"YES\">%s</checksum>", p.Checksum)
		tag(&out, "summary", p.Summary)
		tag(&out, "description", p.Description)
		tag(&out, "packager", "")
		tag(&out, "url", p.URL)
		fmt.Fprintf(&out, "<time file=\"%d\" build=\"%d\"/>", p.FileTime, p.BuildTime)
		fmt.Fprintf(&out, "<size package=\"%d\" installed=\"%d\" archive=\"%d\"/>", p.PackageSize, p.InstalledSize, p.ArchiveSize)
		fmt.Fprintf(&out, "<location href=\"%s\"/>", xmlEscape(p.Location))
		out.WriteString("<format>")
		rpmTag(&out, "license", p.License)
		rpmTag(&out, "vendor", p.Vendor)
		rpmTag(&out, "group", p.Group)
		rpmTag(&out, "buildhost", "")
		rpmTag(&out, "sourcerpm", p.SourceRpm)
		fmt.Fprintf(&out, "<rpm:header-range start=\"%d\" end=\"%d\"/>", p.HeaderStart, p.HeaderEnd)
		dependencyBlock(&out, "rpm:provides", p.Provides)
		dependencyBlock(&out, "rpm:requires", p.Requires)
		dependencyBlock(&out, "rpm:conflicts", p.Conflicts)
		dependencyBlock(&out, "rpm:obsoletes", p.Obsoletes)
		for _, f := range p.Files {
			if f.Kind == "file" {
				tag(&out, "file", f.Path)
			}
		}
		out.WriteString("</format></package>")
	}
	out.WriteString("</metadata>\n")
	return out.String()
}

func generateFilelistsXML(packages []RpmPackage) string {
	var out strings.Builder
	out.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	fmt.Fprintf(&out, "<filelists xmlns=\"http://linux.duke.edu/metadata/filelists\" packages=\"%d\">\n", len(packages))

	for _, p := range packages {
		fmt.Fprintf(&out, "<package pkgid=\"%s\" name=\"%s\" arch=\"%s\">", p.Checksum, xmlEscape(p.Name), xmlEscape(p.Arch))
		fmt.Fprintf(&out, "<version epoch=\"%s\" ver=\"%s\" rel=\"%s\"/>", xmlEscape(p.Epoch), xmlEscape(p.Version), xmlEscape(p.Release))
		for _, f := range p.Files {
			switch f.Kind {
			case "dir":
				fmt.Fprintf(&out, "<file type=\"dir\">%s</file>", xmlEscape(f.Path))
			case "ghost":
				fmt.Fprintf(&out, "<file type=\"ghost\">%s</file>", xmlEscape(f.Path))
			default:
				tag(&out, "file", f.Path)
			}
		}
		out.WriteString("</package>")
	}
	out.WriteString("</filelists>\n")
	return out.String()
}

func generateOtherXML(packages []RpmPackage) string {
	var out strings.Builder
	out.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	fmt.Fprintf(&out, "<otherdata xmlns=\"http://linux.duke.edu/metadata/other\" packages=\"%d\">\n", len(packages))

	for _, p := range packages {
		fmt.Fprintf(&out, "<package pkgid=\"%s\" name=\"%s\" arch=\"%s\">", p.Checksum, xmlEscape(p.Name), xmlEscape(p.Arch))
		fmt.Fprintf(&out, "<version epoch=\"%s\" ver=\"%s\" rel=\"%s\"/>", xmlEscape(p.Epoch), xmlEscape(p.Version), xmlEscape(p.Release))
		for _, c := range p.Changelogs {
			fmt.Fprintf(&out, "<changelog author=\"%s\" date=\"%d\">%s</changelog>", xmlEscape(c.Author), c.Date, xmlEscape(c.Text))
		}
		out.WriteString("</package>")
	}
	out.WriteString("</otherdata>\n")
	return out.String()
}

func generateRepomdXML(primary, filelists, other *MetadataFile) string {
	revision := time.Now().Unix()
	var out strings.Builder
	out.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	out.WriteString("<repomd xmlns=\"http://linux.duke.edu/metadata/repo\">\n")
	fmt.Fprintf(&out, "<revision>%d</revision>\n", revision)

	files := []*MetadataFile{primary, filelists, other}
	for _, file := range files {
		fmt.Fprintf(&out, "<data type=\"%s\">", file.Kind)
		fmt.Fprintf(&out, "<checksum type=\"sha256\">%s</checksum>", file.Checksum)
		fmt.Fprintf(&out, "<open-checksum type=\"sha256\">%s</open-checksum>", file.OpenChecksum)
		fmt.Fprintf(&out, "<location href=\"repodata/%s\"/>", file.Filename)
		fmt.Fprintf(&out, "<timestamp>%d</timestamp>", revision)
		fmt.Fprintf(&out, "<size>%d</size>", file.Size)
		fmt.Fprintf(&out, "<open-size>%d</open-size>", file.OpenSize)
		out.WriteString("</data>")
	}

	out.WriteString("</repomd>\n")
	return out.String()
}

func dependencyBlock(out *strings.Builder, tagName string, dependencies []RpmDependency) {
	if len(dependencies) == 0 {
		return
	}
	fmt.Fprintf(out, "<%s>", tagName)
	for _, dep := range dependencies {
		fmt.Fprintf(out, "<rpm:entry name=\"%s\"", xmlEscape(dep.Name))
		if dep.Flags != "" {
			fmt.Fprintf(out, " flags=\"%s\"", xmlEscape(dep.Flags))
		}
		if dep.Epoch != "" {
			fmt.Fprintf(out, " epoch=\"%s\"", xmlEscape(dep.Epoch))
		}
		if dep.Version != "" {
			fmt.Fprintf(out, " ver=\"%s\"", xmlEscape(dep.Version))
		}
		if dep.Release != "" {
			fmt.Fprintf(out, " rel=\"%s\"", xmlEscape(dep.Release))
		}
		out.WriteString("/>")
	}
	fmt.Fprintf(out, "</%s>", tagName)
}

func tag(out *strings.Builder, tagName, value string) {
	fmt.Fprintf(out, "<%s>%s</%s>", tagName, xmlEscape(value), tagName)
}

func rpmTag(out *strings.Builder, tagName, value string) {
	fmt.Fprintf(out, "<rpm:%s>%s</rpm:%s>", tagName, xmlEscape(value), tagName)
}

func xmlEscape(value string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(value))
	return buf.String()
}
