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

func TestPluginAssetsMatchCanonical(t *testing.T) {
	canonical := "assets"
	mirror := filepath.Join("..", "..", "plugin", "src", "internal", "ui", "assets")
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
