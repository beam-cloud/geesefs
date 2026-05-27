package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadCgroupV2Limit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.max")

	if err := os.WriteFile(path, []byte("max\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := readCgroupV2Limit(path); err != nil || got != 0 {
		t.Fatalf("readCgroupV2Limit(max) = %d, %v; want 0, nil", got, err)
	}

	if err := os.WriteFile(path, []byte("1073741824\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := readCgroupV2Limit(path); err != nil || got != 1073741824 {
		t.Fatalf("readCgroupV2Limit(number) = %d, %v; want 1073741824, nil", got, err)
	}
}
