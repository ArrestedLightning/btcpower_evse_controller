package rotlog

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "app.log")
	f, err := Open(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	line := bytes.Repeat([]byte("x"), 29)
	line = append(line, '\n') // 30 bytes: three lines per file
	for i := 0; i < 20; i++ {
		if _, err := f.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()

	for _, name := range []string{"app.log", "app.log.1", "app.log.2"} {
		fi, err := os.Stat(filepath.Join(dir, "sub", name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if fi.Size() > 100 || fi.Size()%30 != 0 {
			t.Errorf("%s: size %d", name, fi.Size())
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Error("more backups than configured")
	}
}

func TestReopenAppendsAndCountsExistingSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	os.WriteFile(path, bytes.Repeat([]byte("a"), 90), 0o644)
	f, err := Open(path, 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(f, "0123456789abcdef") // would exceed 100: rotates first
	f.Close()
	if b, _ := os.ReadFile(path); string(b) != "0123456789abcdef" {
		t.Fatalf("current file %q", b)
	}
	if fi, _ := os.Stat(path + ".1"); fi == nil || fi.Size() != 90 {
		t.Fatal("old contents not rotated")
	}
}

func TestZeroBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	f, err := Open(path, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("12345678\n"))
	f.Write([]byte("abc\n"))
	f.Close()
	if b, _ := os.ReadFile(path); string(b) != "abc\n" {
		t.Fatalf("got %q", b)
	}
}
