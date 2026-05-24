package preview

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestRenderArchive_Zip(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"README.md", "src/main.go", "src/lib.go", "go.mod"} {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		_, _ = f.Write([]byte("placeholder\n"))
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}

	img, err := renderArchive(context.Background(), "application/zip", buf.Bytes())
	if err != nil {
		t.Fatalf("renderArchive: %v", err)
	}
	if img == nil {
		t.Fatal("renderArchive returned nil image")
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		t.Fatalf("rendered image has empty bounds: %v", b)
	}
}

func TestRenderArchive_TarGz(t *testing.T) {
	t.Parallel()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	for _, name := range []string{"a.txt", "dir/b.txt", "dir/c.txt"} {
		body := []byte("hello")
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o600,
			Size: int64(len(body)),
		}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}

	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	if _, err := gz.Write(tarBuf.Bytes()); err != nil {
		t.Fatalf("gz write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}

	img, err := renderArchive(context.Background(), "application/gzip", gzBuf.Bytes())
	if err != nil {
		t.Fatalf("renderArchive: %v", err)
	}
	if img == nil {
		t.Fatal("renderArchive returned nil image")
	}
}

func TestRenderArchive_CorruptZip(t *testing.T) {
	t.Parallel()
	_, err := renderArchive(context.Background(), "application/zip", []byte("not a zip"))
	if err == nil {
		t.Fatal("expected error for corrupt zip")
	}
	// Corrupt input must NOT masquerade as ErrUnsupportedMime —
	// otherwise the worker would silently drop a real corruption
	// signal.
	if errors.Is(err, ErrUnsupportedMime) {
		t.Fatalf("corrupt zip should not be ErrUnsupportedMime, got %v", err)
	}
}

func TestRenderArchive_UnknownMime(t *testing.T) {
	t.Parallel()
	_, err := renderArchive(context.Background(), "application/x-not-an-archive", []byte("..."))
	if !errors.Is(err, ErrUnsupportedMime) {
		t.Fatalf("expected ErrUnsupportedMime for unknown archive mime, got %v", err)
	}
}

// TestListZipEntries_HonoursEntryCap exercises the listZipEntries
// cap so a crafted archive with hundreds of thousands of entries
// can't blow up the worker's memory + sort budget. The entry count
// just over the cap is enough to verify the early break path
// without baking a slow test.
func TestListZipEntries_HonoursEntryCap(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	const entries = archiveMaxEntries * 4
	for i := 0; i < entries; i++ {
		f, err := zw.Create(fmt.Sprintf("entry-%05d.txt", i))
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		_, _ = f.Write([]byte("x"))
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}

	got, total, err := listZipEntries(buf.Bytes())
	if err != nil {
		t.Fatalf("listZipEntries: %v", err)
	}
	if len(got) != archiveMaxEntries {
		t.Errorf("len(got) = %d, want exactly the cap %d", len(got), archiveMaxEntries)
	}
	if total != entries {
		t.Errorf("total = %d, want %d (the real entry count, not the cap)", total, entries)
	}
}
