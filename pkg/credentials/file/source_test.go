package file

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sparksq/sparkroute/pkg/credentials"
)

func TestSourceResolvesRawAndRotatedJSONField(t *testing.T) {
	t.Parallel()

	root := privateDirectory(t)
	rawPath := filepath.Join(root, "raw")
	writePrivateFile(t, rawPath, []byte("raw-secret"))
	source, err := New(Options{Roots: []string{root}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	material, err := source.Resolve(
		context.Background(),
		credentials.Ref("file://"+rawPath),
	)
	if err != nil {
		t.Fatalf("Resolve() raw error = %v", err)
	}
	if string(material.Value) != "raw-secret" || material.Version == "" {
		t.Fatalf("raw material = %#v", material)
	}

	jsonPath := filepath.Join(root, "document.json")
	writePrivateFile(t, jsonPath, []byte(`{"api_key":"first"}`))
	ref := credentials.Ref("file://" + jsonPath + "#api_key")
	first, err := source.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve() JSON error = %v", err)
	}
	replacement := filepath.Join(root, "replacement")
	writePrivateFile(t, replacement, []byte(`{"api_key":"second"}`))
	if err := os.Rename(replacement, jsonPath); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	second, err := source.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve() rotated JSON error = %v", err)
	}
	if string(first.Value) != "first" ||
		string(second.Value) != "second" ||
		first.Version == second.Version {
		t.Fatalf("rotation = first:%#v second:%#v", first, second)
	}
}

func TestSourceRejectsEscapesAndInsecurePermissions(t *testing.T) {
	t.Parallel()

	root := privateDirectory(t)
	outside := filepath.Join(privateDirectory(t), "outside")
	writePrivateFile(t, outside, []byte("secret"))
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	source, err := New(Options{Roots: []string{root}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := source.Resolve(
		context.Background(),
		credentials.Ref("file://"+link),
	); err == nil || !strings.Contains(err.Error(), "outside configured roots") {
		t.Fatalf("Resolve() escape error = %v", err)
	}

	insecure := filepath.Join(root, "insecure")
	writePrivateFile(t, insecure, []byte("secret"))
	if err := os.Chmod(insecure, 0o644); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	if _, err := source.Resolve(
		context.Background(),
		credentials.Ref("file://"+insecure),
	); err == nil || !strings.Contains(err.Error(), "grant access") {
		t.Fatalf("Resolve() permissions error = %v", err)
	}
}

func TestSourceSupportsProjectedVolumeSymlinkRotation(t *testing.T) {
	t.Parallel()

	root := privateDirectory(t)
	firstDirectory := filepath.Join(root, "..data-first")
	secondDirectory := filepath.Join(root, "..data-second")
	for _, directory := range []string{firstDirectory, secondDirectory} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatalf("Mkdir() error = %v", err)
		}
	}
	writePrivateFile(t, filepath.Join(firstDirectory, "token"), []byte("first"))
	writePrivateFile(t, filepath.Join(secondDirectory, "token"), []byte("second"))
	dataLink := filepath.Join(root, "..data")
	if err := os.Symlink(firstDirectory, dataLink); err != nil {
		t.Fatalf("Symlink() data error = %v", err)
	}
	if err := os.Symlink(filepath.Join("..data", "token"), filepath.Join(root, "token")); err != nil {
		t.Fatalf("Symlink() token error = %v", err)
	}
	source, err := New(Options{Roots: []string{root}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ref := credentials.Ref("file://" + filepath.Join(root, "token"))
	first, err := source.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve() first error = %v", err)
	}
	replacement := filepath.Join(root, "..data-new")
	if err := os.Symlink(secondDirectory, replacement); err != nil {
		t.Fatalf("Symlink() replacement error = %v", err)
	}
	if err := os.Rename(replacement, dataLink); err != nil {
		t.Fatalf("Rename() data link error = %v", err)
	}
	second, err := source.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve() second error = %v", err)
	}
	if string(first.Value) != "first" || string(second.Value) != "second" {
		t.Fatalf("projected rotation = first:%q second:%q", first.Value, second.Value)
	}
}

func privateDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("Chmod() directory error = %v", err)
	}
	return directory
}

func writePrivateFile(t *testing.T, path string, value []byte) {
	t.Helper()
	if err := os.WriteFile(path, value, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}
