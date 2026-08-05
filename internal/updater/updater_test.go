package updater

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type zipEntry struct {
	name string
	data string
	mode os.FileMode
}

func svd(device string) string {
	return `<?xml version="1.0"?><device><name>` + device + `</name></device>`
}

func makePack(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		w, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeConfig(t *testing.T, dir string, packs []Pack) string {
	t.Helper()
	data, err := json.Marshal(Config{Packs: packs})
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "packs.json")
	if err := os.WriteFile(name, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return name
}

func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	result := make(map[string]string)
	err := filepath.WalkDir(dir, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, name)
		if err != nil {
			return err
		}
		result[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestRunEndToEndDeterministic(t *testing.T) {
	packA := makePack(t,
		zipEntry{name: "CMSIS/SVD/PY32B.SVD", data: svd("PY32B")},
		zipEntry{name: "CMSIS/SVD/PY32A.svd", data: svd("PY32A")},
		zipEntry{name: "CMSIS/SVD/PY32A.SFR", data: "future comments"},
	)
	packB := makePack(t,
		zipEntry{name: "Files/CMSIS/SVD/PY32C.svd", data: svd("PY32C")},
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != userAgent {
			t.Errorf("User-Agent = %q, want %q", got, userAgent)
		}
		switch r.URL.Path {
		case "/a.pack":
			_, _ = w.Write(packA)
		case "/b.pack":
			_, _ = w.Write(packB)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	config := writeConfig(t, dir, []Pack{
		{Name: "B", URL: server.URL + "/b.pack", SHA256: digest(packB)},
		{Name: "A", URL: server.URL + "/a.pack", SHA256: digest(packA)},
	})
	output := filepath.Join(dir, "svd")
	opts := Options{ConfigPath: config, OutputDir: output, Client: server.Client()}
	if err := Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	first := snapshot(t, output)
	if err := Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	second := snapshot(t, output)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("second run changed output\nfirst: %#v\nsecond: %#v", first, second)
	}

	for _, name := range []string{"py32a.svd", "py32b.svd", "py32c.svd", "manifest.json"} {
		if _, ok := first[name]; !ok {
			t.Errorf("missing output %s", name)
		}
	}
	if _, ok := first["py32a.sfr"]; ok {
		t.Error("SFR sidecar was unexpectedly published")
	}
	var manifest Manifest
	if err := json.Unmarshal([]byte(first["manifest.json"]), &manifest); err != nil {
		t.Fatal(err)
	}
	if got := []string{manifest.Packs[0].Name, manifest.Packs[1].Name}; !reflect.DeepEqual(got, []string{"A", "B"}) {
		t.Errorf("pack order = %v", got)
	}
	if got := []string{manifest.Files[0].Name, manifest.Files[1].Name, manifest.Files[2].Name}; !reflect.DeepEqual(got, []string{"py32a.svd", "py32b.svd", "py32c.svd"}) {
		t.Errorf("file order = %v", got)
	}
}

func TestRunFailurePreservesOutput(t *testing.T) {
	good := makePack(t, zipEntry{name: "device.svd", data: svd("Good")})
	tests := []struct {
		name        string
		status      int
		pack        []byte
		checksum    string
		maxPack     int64
		wantInError string
	}{
		{name: "HTTP", status: http.StatusNotFound, wantInError: "HTTP 404"},
		{name: "checksum", status: http.StatusOK, pack: good, checksum: strings.Repeat("0", 64), wantInError: "SHA-256 mismatch"},
		{name: "pack size", status: http.StatusOK, pack: good, checksum: digest(good), maxPack: 4, wantInError: "larger than"},
		{name: "malformed ZIP", status: http.StatusOK, pack: []byte("not zip"), wantInError: "open archive"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write(test.pack)
			}))
			defer server.Close()
			dir := t.TempDir()
			output := filepath.Join(dir, "svd")
			if err := os.Mkdir(output, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(output, "old.svd"), []byte("unchanged"), 0o644); err != nil {
				t.Fatal(err)
			}
			before := snapshot(t, output)
			config := writeConfig(t, dir, []Pack{{Name: "test", URL: server.URL, SHA256: test.checksum}})
			err := Run(context.Background(), Options{
				ConfigPath:  config,
				OutputDir:   output,
				Client:      server.Client(),
				MaxPackSize: test.maxPack,
			})
			if err == nil || !strings.Contains(err.Error(), test.wantInError) {
				t.Fatalf("error = %v, want substring %q", err, test.wantInError)
			}
			if after := snapshot(t, output); !reflect.DeepEqual(before, after) {
				t.Fatalf("output changed after failure: before=%v after=%v", before, after)
			}
		})
	}
}

func TestExtractRejectsInvalidArchives(t *testing.T) {
	tests := []struct {
		name      string
		entries   []zipEntry
		entryMax  uint64
		expandMax uint64
		want      string
	}{
		{name: "empty", entries: []zipEntry{{name: "readme.txt", data: "x"}}, want: "no SVD"},
		{name: "traversal", entries: []zipEntry{{name: "../bad.svd", data: svd("Bad")}}, want: "unsafe archive path"},
		{name: "absolute", entries: []zipEntry{{name: "/bad.svd", data: svd("Bad")}}, want: "unsafe archive path"},
		{name: "backslash", entries: []zipEntry{{name: `CMSIS\\bad.svd`, data: svd("Bad")}}, want: "unsafe archive path"},
		{name: "symlink", entries: []zipEntry{{name: "bad.svd", data: "target", mode: os.ModeSymlink | 0o777}}, want: "symlink"},
		{name: "wrong root", entries: []zipEntry{{name: "bad.svd", data: "<peripheral><name>x</name></peripheral>"}}, want: "root element"},
		{name: "missing device", entries: []zipEntry{{name: "bad.svd", data: "<device/>"}}, want: "device name is empty"},
		{name: "malformed XML", entries: []zipEntry{{name: "bad.svd", data: "<device>"}}, want: "invalid SVD"},
		{name: "entry limit", entries: []zipEntry{{name: "bad.svd", data: svd("Device")}}, entryMax: 4, want: "larger than"},
		{name: "expanded limit", entries: []zipEntry{{name: "a.txt", data: "1234"}, {name: "good.svd", data: svd("Good")}}, entryMax: 1024, expandMax: 4, want: "expands beyond"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entryMax := test.entryMax
			if entryMax == 0 {
				entryMax = 1024
			}
			expandMax := test.expandMax
			if expandMax == 0 {
				expandMax = 4096
			}
			_, err := extract("test", makePack(t, test.entries...), entryMax, expandMax)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestRunDuplicateOutputs(t *testing.T) {
	content := svd("Same")
	tests := []struct {
		name    string
		second  string
		wantErr bool
	}{
		{name: "identical", second: content},
		{name: "conflicting", second: svd("Different"), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			packs := map[string][]byte{
				"/a": makePack(t, zipEntry{name: "one/DEVICE.svd", data: content}),
				"/b": makePack(t, zipEntry{name: "two/device.SVD", data: test.second}),
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(packs[r.URL.Path])
			}))
			defer server.Close()
			dir := t.TempDir()
			config := writeConfig(t, dir, []Pack{
				{Name: "a", URL: server.URL + "/a", SHA256: digest(packs["/a"])},
				{Name: "b", URL: server.URL + "/b", SHA256: digest(packs["/b"])},
			})
			err := Run(context.Background(), Options{ConfigPath: config, OutputDir: filepath.Join(dir, "svd"), Client: server.Client()})
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "output collision") {
					t.Fatalf("error = %v, want collision", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLoadConfigValidation(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "unknown field", data: `{"packs":[],"extra":true}`, want: "unknown field"},
		{name: "empty", data: `{"packs":[]}`, want: "no packs"},
		{name: "bad URL", data: `{"packs":[{"name":"a","url":"file:///tmp/a"}]}`, want: "invalid URL"},
		{name: "bad checksum", data: `{"packs":[{"name":"a","url":"https://example.com/a","sha256":"bad"}]}`, want: "invalid SHA-256"},
		{name: "duplicate name", data: `{"packs":[{"name":"a","url":"https://example.com/a"},{"name":"a","url":"https://example.com/b"}]}`, want: "duplicate pack name"},
		{name: "duplicate URL", data: `{"packs":[{"name":"a","url":"https://example.com/a"},{"name":"b","url":"https://example.com/a"}]}`, want: "duplicate pack URL"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(name, []byte(test.data), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := loadConfig(name)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}
