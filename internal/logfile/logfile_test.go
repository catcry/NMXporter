package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "exporter.log")
	w, err := New(p, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 39) + "\n" // 40 bytes
	for i := 0; i < 8; i++ {               // 320 bytes → several rotations
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"exporter.log", "exporter.log.1", "exporter.log.2"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
		if info.Size() > 100 {
			t.Errorf("%s is %d bytes, above the 100 byte limit", name, info.Size())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "exporter.log.3")); err == nil {
		t.Error("only 2 rotated files must be kept")
	}

	// Reopening appends to the existing file and keeps its size.
	w2, _ := New(p, 100, 2)
	before, _ := os.Stat(p)
	w2.Write([]byte("y\n"))
	after, _ := os.Stat(p)
	if after.Size() != before.Size()+2 {
		t.Errorf("reopen should append: %d -> %d", before.Size(), after.Size())
	}
}
