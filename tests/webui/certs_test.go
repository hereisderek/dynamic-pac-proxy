package webui_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/derekhud/dynamic-pac-proxy/internal/config"
	"github.com/derekhud/dynamic-pac-proxy/internal/webui"
)

func TestListCertFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"charles-root.pem", "README.md", ".hidden", "b.crt", "a.crt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	files := webui.ListCertFiles(dir)
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	want := []string{"a.crt", "b.crt", "charles-root.pem"}
	if len(names) != len(want) {
		t.Fatalf("ListCertFiles() = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("ListCertFiles() = %v, want %v", names, want)
		}
	}
}

func TestListCertFilesMissingDir(t *testing.T) {
	if files := webui.ListCertFiles(filepath.Join(t.TempDir(), "does-not-exist")); files != nil {
		t.Fatalf("expected nil for a missing dir, got %v", files)
	}
}

func newTestCfgStoreWithCertsDir(base string) *config.Store {
	return config.NewStore(config.FileConfig{CertsDir: "certs"}, filepath.Join(base, "config.yaml"))
}

func TestCertsIndexHandler(t *testing.T) {
	base := t.TempDir()
	certsDir := filepath.Join(base, "certs")
	if err := os.MkdirAll(certsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(certsDir, "charles-root.pem"), []byte("cert-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgStore := newTestCfgStoreWithCertsDir(base)
	srv := httptest.NewServer(webui.CertsIndexHandler(cfgStore))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/certs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(bodyBytes)
	if !strings.Contains(body, "charles-root.pem") {
		t.Fatalf("expected index page to list charles-root.pem, got: %s", body)
	}
	if !strings.Contains(body, `href="/certs/charles-root.pem"`) {
		t.Fatalf("expected a download link for charles-root.pem, got: %s", body)
	}
}

func TestCertsFileHandler(t *testing.T) {
	base := t.TempDir()
	certsDir := filepath.Join(base, "certs")
	if err := os.MkdirAll(certsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(certsDir, "charles-root.pem"), []byte("cert-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgStore := newTestCfgStoreWithCertsDir(base)
	srv := httptest.NewServer(webui.CertsFileHandler(cfgStore))
	defer srv.Close()

	t.Run("serves a known cert with a recognizable content type", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/certs/charles-root.pem")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/x-x509-ca-cert" {
			t.Fatalf("Content-Type = %q", ct)
		}
	})

	t.Run("404s for README.md", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/certs/README.md")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("blocks path traversal", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/certs/..%2Fconfig.yaml")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("path traversal should not succeed, got 200")
		}
	})
}
