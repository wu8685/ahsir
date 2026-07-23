package ui

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func regularFiles(t *testing.T, root string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(names)
	return names
}

func findFullRepositoryRoot(start string) (string, bool) {
	candidate, err := filepath.Abs(start)
	if err != nil {
		return "", false
	}
	for {
		if isRegularFile(filepath.Join(candidate, "go.mod")) &&
			isDirectory(filepath.Join(candidate, "internal", "ui", "assets")) &&
			isRegularFile(filepath.Join(candidate, "plugin", ".claude-plugin", "plugin.json")) {
			return candidate, true
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return "", false
		}
		candidate = parent
	}
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func TestFindFullRepositoryRootFromCanonicalAndCopiedPluginPackages(t *testing.T) {
	repositoryRoot := t.TempDir()
	for _, path := range []string{
		filepath.Join(repositoryRoot, "internal", "ui", "assets"),
		filepath.Join(repositoryRoot, "plugin", "src", "internal", "ui"),
		filepath.Join(repositoryRoot, "plugin", ".claude-plugin"),
	} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}
	for _, path := range []string{
		filepath.Join(repositoryRoot, "go.mod"),
		filepath.Join(repositoryRoot, "plugin", ".claude-plugin", "plugin.json"),
	} {
		if err := os.WriteFile(path, []byte("test marker\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	for _, start := range []string{
		filepath.Join(repositoryRoot, "internal", "ui"),
		filepath.Join(repositoryRoot, "plugin", "src", "internal", "ui"),
	} {
		got, ok := findFullRepositoryRoot(start)
		if !ok {
			t.Fatalf("findFullRepositoryRoot(%s) did not find the repository", start)
		}
		if got != repositoryRoot {
			t.Fatalf("findFullRepositoryRoot(%s) = %s, want %s", start, got, repositoryRoot)
		}
	}
}

func TestFindFullRepositoryRootRejectsStandalonePluginSource(t *testing.T) {
	standaloneRoot := t.TempDir()
	packageDir := filepath.Join(standaloneRoot, "internal", "ui")
	if err := os.MkdirAll(filepath.Join(packageDir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(standaloneRoot, "go.mod"), []byte("module github.com/wu8685/ahsir\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got, ok := findFullRepositoryRoot(packageDir); ok {
		t.Fatalf("standalone plugin source resolved repository root %s", got)
	}
}

func TestPluginAssetsMatchCanonical(t *testing.T) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	repositoryRoot, ok := findFullRepositoryRoot(workingDirectory)
	if !ok {
		t.Skip("asset parity requires the full ahsir repository; standalone plugin source has no canonical asset tree")
	}
	canonical := filepath.Join(repositoryRoot, "internal", "ui", "assets")
	mirror := filepath.Join(repositoryRoot, "plugin", "src", "internal", "ui", "assets")
	want, got := regularFiles(t, canonical), regularFiles(t, mirror)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("asset names = %v, want %v", got, want)
	}
	for _, name := range want {
		a, err := os.ReadFile(filepath.Join(canonical, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(mirror, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("plugin asset differs: %s", name)
		}
	}
}
