// Package updater downloads Puya CMSIS packs and publishes their SVD files.
package updater

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultMaxPackSize     = int64(128 << 20)
	defaultMaxEntrySize    = uint64(32 << 20)
	defaultMaxExpandedSize = uint64(512 << 20)
	userAgent              = "tinygo-org/py32-svd updater"
)

// Options configures an update.
type Options struct {
	ConfigPath string
	OutputDir  string
	Client     *http.Client

	// Resource limits are primarily configurable for tests. Zero selects the
	// conservative production default.
	MaxPackSize     int64
	MaxEntrySize    uint64
	MaxExpandedSize uint64
}

// Config is the checked-in list of required CMSIS packs.
type Config struct {
	Packs []Pack `json:"packs"`
}

// Pack identifies one required CMSIS pack. SHA256 may be empty only while an
// upstream archive is unavailable and therefore cannot yet be hashed.
type Pack struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256,omitempty"`
}

// Manifest records the exact provenance of generated files.
type Manifest struct {
	Packs []ManifestPack `json:"packs"`
	Files []ManifestFile `json:"files"`
}

type ManifestPack struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type ManifestFile struct {
	Name                   string   `json:"name"`
	Device                 string   `json:"device"`
	Pack                   string   `json:"pack"`
	ArchivePath            string   `json:"archive_path"`
	Headers                []string `json:"headers,omitempty"`
	HeaderEnumeratedValues int      `json:"header_enumerated_values,omitempty"`
	SHA256                 string   `json:"sha256"`
}

type generatedFile struct {
	manifest ManifestFile
	data     []byte
}

type archiveSource struct {
	path string
	data []byte
}

// Run performs an all-or-nothing update of Options.OutputDir.
func Run(ctx context.Context, opts Options) error {
	if opts.ConfigPath == "" {
		return errors.New("config path is required")
	}
	if opts.OutputDir == "" {
		return errors.New("output directory is required")
	}
	cleanOutput := filepath.Clean(opts.OutputDir)
	if cleanOutput == "." || cleanOutput == string(filepath.Separator) {
		return fmt.Errorf("refusing unsafe output directory %q", opts.OutputDir)
	}

	config, err := loadConfig(opts.ConfigPath)
	if err != nil {
		return err
	}
	applyDefaults(&opts)
	if opts.MaxPackSize < 1 || opts.MaxEntrySize < 1 || opts.MaxExpandedSize < 1 {
		return errors.New("resource limits must be positive")
	}

	manifest := Manifest{}
	files := make(map[string]generatedFile)
	for _, pack := range config.Packs {
		archive, digest, err := download(ctx, opts.Client, pack, opts.MaxPackSize)
		if err != nil {
			return err
		}
		manifest.Packs = append(manifest.Packs, ManifestPack{
			Name:   pack.Name,
			URL:    pack.URL,
			SHA256: digest,
		})

		extracted, err := extract(pack.Name, archive, opts.MaxEntrySize, opts.MaxExpandedSize)
		if err != nil {
			return fmt.Errorf("pack %s: %w", pack.Name, err)
		}
		for _, candidate := range extracted {
			if existing, ok := files[candidate.manifest.Name]; ok {
				if !bytes.Equal(existing.data, candidate.data) {
					return fmt.Errorf("output collision for %s between %s:%s and %s:%s",
						candidate.manifest.Name,
						existing.manifest.Pack, existing.manifest.ArchivePath,
						candidate.manifest.Pack, candidate.manifest.ArchivePath)
				}
				continue
			}
			files[candidate.manifest.Name] = candidate
		}
	}

	sort.Slice(manifest.Packs, func(i, j int) bool {
		return manifest.Packs[i].Name < manifest.Packs[j].Name
	})
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		manifest.Files = append(manifest.Files, files[name].manifest)
	}

	return publish(opts.OutputDir, files, manifest)
}

func applyDefaults(opts *Options) {
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 3 * time.Minute}
	}
	if opts.MaxPackSize == 0 {
		opts.MaxPackSize = defaultMaxPackSize
	}
	if opts.MaxEntrySize == 0 {
		opts.MaxEntrySize = defaultMaxEntrySize
	}
	if opts.MaxExpandedSize == 0 {
		opts.MaxExpandedSize = defaultMaxExpandedSize
	}
}

func loadConfig(name string) (Config, error) {
	f, err := os.Open(name)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.DisallowUnknownFields()
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return Config{}, fmt.Errorf("decode config: trailing data: %w", err)
	}
	if len(config.Packs) == 0 {
		return Config{}, errors.New("config contains no packs")
	}

	seenNames := make(map[string]bool)
	seenURLs := make(map[string]bool)
	for i := range config.Packs {
		pack := &config.Packs[i]
		pack.Name = strings.TrimSpace(pack.Name)
		pack.URL = strings.TrimSpace(pack.URL)
		pack.SHA256 = strings.ToLower(strings.TrimSpace(pack.SHA256))
		if pack.Name == "" || pack.URL == "" {
			return Config{}, fmt.Errorf("pack %d requires name and URL", i)
		}
		parsed, err := url.Parse(pack.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return Config{}, fmt.Errorf("pack %s has invalid URL %q", pack.Name, pack.URL)
		}
		if pack.SHA256 != "" {
			decoded, err := hex.DecodeString(pack.SHA256)
			if err != nil || len(decoded) != sha256.Size {
				return Config{}, fmt.Errorf("pack %s has invalid SHA-256", pack.Name)
			}
		}
		if seenNames[pack.Name] {
			return Config{}, fmt.Errorf("duplicate pack name %q", pack.Name)
		}
		if seenURLs[pack.URL] {
			return Config{}, fmt.Errorf("duplicate pack URL %q", pack.URL)
		}
		seenNames[pack.Name] = true
		seenURLs[pack.URL] = true
	}
	sort.Slice(config.Packs, func(i, j int) bool {
		return config.Packs[i].Name < config.Packs[j].Name
	})
	return config, nil
}

func download(ctx context.Context, client *http.Client, pack Pack, maxSize int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pack.URL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("pack %s: create request: %w", pack.Name, err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("pack %s: download %s: %w", pack.Name, pack.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("pack %s: download %s: HTTP %s", pack.Name, pack.URL, resp.Status)
	}
	if resp.ContentLength > maxSize {
		return nil, "", fmt.Errorf("pack %s: archive is larger than %d bytes", pack.Name, maxSize)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return nil, "", fmt.Errorf("pack %s: read response: %w", pack.Name, err)
	}
	if int64(len(data)) > maxSize {
		return nil, "", fmt.Errorf("pack %s: archive is larger than %d bytes", pack.Name, maxSize)
	}
	digestBytes := sha256.Sum256(data)
	digest := hex.EncodeToString(digestBytes[:])
	if pack.SHA256 != "" && digest != pack.SHA256 {
		return nil, "", fmt.Errorf("pack %s: SHA-256 mismatch: got %s, want %s", pack.Name, digest, pack.SHA256)
	}
	return data, digest, nil
}

func extract(packName string, archive []byte, maxEntrySize, maxExpandedSize uint64) ([]generatedFile, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}

	seenPaths := make(map[string]bool)
	var expanded uint64
	var svds []archiveSource
	var headers []headerSource
	for _, entry := range zr.File {
		clean, err := safeArchivePath(entry.Name)
		if err != nil {
			return nil, err
		}
		if seenPaths[clean] {
			return nil, fmt.Errorf("duplicate archive path %q", entry.Name)
		}
		seenPaths[clean] = true
		if entry.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("archive contains symlink %q", entry.Name)
		}
		if entry.FileInfo().IsDir() {
			continue
		}
		if entry.UncompressedSize64 > maxEntrySize {
			return nil, fmt.Errorf("archive entry %q is larger than %d bytes", entry.Name, maxEntrySize)
		}
		if entry.UncompressedSize64 > maxExpandedSize-expanded {
			return nil, fmt.Errorf("archive expands beyond %d bytes", maxExpandedSize)
		}
		expanded += entry.UncompressedSize64
		isSVD := strings.EqualFold(path.Ext(clean), ".svd")
		isHeader := isCMSISDeviceHeader(clean)
		if !isSVD && !isHeader {
			continue
		}

		data, err := readZipEntry(entry, maxEntrySize)
		if err != nil {
			return nil, err
		}
		if isSVD {
			svds = append(svds, archiveSource{path: clean, data: data})
		} else {
			headers = append(headers, headerSource{Path: clean, Data: data})
		}
	}
	if len(svds) == 0 {
		return nil, errors.New("archive contains no SVD files")
	}
	sort.Slice(svds, func(i, j int) bool {
		return svds[i].path < svds[j].path
	})
	sort.Slice(headers, func(i, j int) bool {
		return headers[i].Path < headers[j].Path
	})
	headersBySVD := associateDeviceHeaders(svds, headers)

	files := make([]generatedFile, 0, len(svds))
	for _, source := range svds {
		data, err := patchSVD(source.data)
		if err != nil {
			return nil, fmt.Errorf("patch SVD %q: %w", source.path, err)
		}
		associatedHeaders := headersBySVD[source.path]
		data, valueCount, err := enrichSVDFromHeaders(data, associatedHeaders)
		if err != nil {
			return nil, fmt.Errorf("enrich SVD %q: %w", source.path, err)
		}
		device, err := validateSVD(data)
		if err != nil {
			return nil, fmt.Errorf("invalid SVD %q: %w", source.path, err)
		}
		digest := sha256.Sum256(data)
		name := strings.ToLower(path.Base(source.path))
		headerPaths := make([]string, len(associatedHeaders))
		for i := range associatedHeaders {
			headerPaths[i] = associatedHeaders[i].Path
		}
		files = append(files, generatedFile{
			manifest: ManifestFile{
				Name:                   name,
				Device:                 device,
				Pack:                   packName,
				ArchivePath:            source.path,
				Headers:                headerPaths,
				HeaderEnumeratedValues: valueCount,
				SHA256:                 hex.EncodeToString(digest[:]),
			},
			data: data,
		})
	}
	return files, nil
}

func isCMSISDeviceHeader(name string) bool {
	lower := strings.ToLower(name)
	withRoot := "/" + lower
	base := path.Base(lower)
	return strings.Contains(withRoot, "/cmsis/device/") &&
		strings.Contains(withRoot, "/include/") &&
		strings.HasPrefix(base, "py32") && strings.EqualFold(path.Ext(base), ".h")
}

// associateDeviceHeaders assigns each concrete device header to the SVD with
// the longest matching basename prefix. This distinguishes, for example,
// PY32F040xx, PY32F040Cxx, PY32F040Exx, and PY32F040EPxx without relying on a
// particular package directory layout.
func associateDeviceHeaders(svds []archiveSource, headers []headerSource) map[string][]headerSource {
	prefixes := make([]string, len(svds))
	for i := range svds {
		stem := strings.TrimSuffix(path.Base(svds[i].path), path.Ext(svds[i].path))
		stem = strings.ToLower(strings.TrimSpace(stem))
		if strings.HasSuffix(stem, "xx") {
			stem = stem[:len(stem)-2]
		}
		prefixes[i] = stem
	}

	result := make(map[string][]headerSource)
	for _, header := range headers {
		stem := strings.TrimSuffix(path.Base(header.Path), path.Ext(header.Path))
		stem = strings.ToLower(strings.TrimSpace(stem))
		best := -1
		for i, prefix := range prefixes {
			if prefix != "" && strings.HasPrefix(stem, prefix) && (best < 0 || len(prefix) > len(prefixes[best])) {
				best = i
			}
		}
		if best >= 0 {
			path := svds[best].path
			result[path] = append(result[path], header)
		}
	}
	return result
}

func safeArchivePath(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, '\x00') || strings.Contains(name, "\\") {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	clean := path.Clean(name)
	if clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || clean != strings.TrimSuffix(name, "/") {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	return clean, nil
}

func readZipEntry(entry *zip.File, maxSize uint64) ([]byte, error) {
	r, err := entry.Open()
	if err != nil {
		return nil, fmt.Errorf("open archive entry %q: %w", entry.Name, err)
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, int64(maxSize)+1))
	if err != nil {
		return nil, fmt.Errorf("read archive entry %q: %w", entry.Name, err)
	}
	if uint64(len(data)) > maxSize {
		return nil, fmt.Errorf("archive entry %q is larger than %d bytes", entry.Name, maxSize)
	}
	return data, nil
}

func validateSVD(data []byte) (string, error) {
	var device struct {
		XMLName xml.Name
		Name    string `xml:"name"`
	}
	if err := xml.Unmarshal(data, &device); err != nil {
		return "", err
	}
	if device.XMLName.Local != "device" {
		return "", fmt.Errorf("root element is <%s>, want <device>", device.XMLName.Local)
	}
	device.Name = strings.TrimSpace(device.Name)
	if device.Name == "" {
		return "", errors.New("device name is empty")
	}
	return device.Name, nil
}

func publish(outputDir string, files map[string]generatedFile, manifest Manifest) error {
	outputDir = filepath.Clean(outputDir)
	parent := filepath.Dir(outputDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create output parent: %w", err)
	}
	stage, err := os.MkdirTemp(parent, ".svd-stage-")
	if err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}
	stageExists := true
	defer func() {
		if stageExists {
			_ = os.RemoveAll(stage)
		}
	}()

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(stage, name), files[name].data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	manifestData = append(manifestData, '\n')
	if err := os.WriteFile(filepath.Join(stage, "manifest.json"), manifestData, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	backup := ""
	if _, err := os.Stat(outputDir); err == nil {
		backup, err = reservePath(parent, ".svd-backup-")
		if err != nil {
			return err
		}
		if err := os.Rename(outputDir, backup); err != nil {
			return fmt.Errorf("move old output aside: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect old output: %w", err)
	}

	if err := os.Rename(stage, outputDir); err != nil {
		if backup != "" {
			if restoreErr := os.Rename(backup, outputDir); restoreErr != nil {
				return fmt.Errorf("publish output: %v; restore old output: %w", err, restoreErr)
			}
		}
		return fmt.Errorf("publish output: %w", err)
	}
	stageExists = false
	if backup != "" {
		if err := os.RemoveAll(backup); err != nil {
			return fmt.Errorf("remove output backup: %w", err)
		}
	}
	return nil
}

func reservePath(parent, pattern string) (string, error) {
	name, err := os.MkdirTemp(parent, pattern)
	if err != nil {
		return "", fmt.Errorf("reserve backup path: %w", err)
	}
	if err := os.Remove(name); err != nil {
		return "", fmt.Errorf("reserve backup path: %w", err)
	}
	return name, nil
}
