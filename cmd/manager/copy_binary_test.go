package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyBinary(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
	}{
		{"non-empty file", []byte("ELF binary content")},
		{"empty file", []byte{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "src")
			if err := os.WriteFile(src, tt.content, 0o644); err != nil {
				t.Fatal(err)
			}

			dst := filepath.Join(t.TempDir(), "dst")
			if err := copyBinary(src, dst); err != nil {
				t.Fatalf("copyBinary: %v", err)
			}

			got, err := os.ReadFile(dst)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(tt.content) {
				t.Errorf("content mismatch: got %q, want %q", got, tt.content)
			}

			info, err := os.Stat(dst)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm()&0o111 == 0 {
				t.Errorf("destination not executable: mode %v", info.Mode())
			}
		})
	}
}

func TestStageCACertBundle(t *testing.T) {
	bundle := []byte("-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----\n")
	src := filepath.Join(t.TempDir(), "ca-certificates.crt")
	if err := os.WriteFile(src, bundle, 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "ca-certificates.crt")
	if err := stageCACertBundle(src, dst); err != nil {
		t.Fatalf("stageCACertBundle: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(bundle) {
		t.Errorf("content mismatch: got %q, want %q", got, bundle)
	}
}

func TestStageCACertBundleMissingSourceFails(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "ca-certificates.crt")
	if err := stageCACertBundle("/nonexistent/ca-certificates.crt", dst); err == nil {
		t.Fatal("expected error for missing source, got nil")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("expected no destination file to be written, stat err = %v", err)
	}
}

func TestCopyBinarySourceNotFound(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "dst")
	err := copyBinary("/nonexistent/path/to/binary", dst)
	if err == nil {
		t.Fatal("expected error for missing source, got nil")
	}
}

func TestCopyBinaryDestinationUnwritable(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := copyBinary(src, "/nonexistent/dir/dst")
	if err == nil {
		t.Fatal("expected error for unwritable destination, got nil")
	}
}
