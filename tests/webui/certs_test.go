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
	"github.com/derekhud/dynamic-pac-proxy/internal/mitm"
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

// TestListCertFilesExcludesCAKey guards against the local CA's private
// key (internal/mitm.CAKeyFileName) ever being listed on /certs. It's
// written to config.Store.ConfigDir rather than CertsDir precisely so it
// never gets served — but certs_dir is configured independently and can
// resolve to the same directory (e.g. certs_dir: "."), so the exclusion
// has to hold regardless of directory layout, not just be a consequence
// of where the file happens to live.
func TestListCertFilesExcludesCAKey(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"charles-root.pem", mitm.CAKeyFileName} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	files := webui.ListCertFiles(dir)
	for _, f := range files {
		if strings.EqualFold(f.Name, mitm.CAKeyFileName) {
			t.Fatalf("ListCertFiles() must never include the CA private key, got %v", files)
		}
	}
	if len(files) != 1 || files[0].Name != "charles-root.pem" {
		t.Fatalf("ListCertFiles() = %v, want only charles-root.pem", files)
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

	t.Run("404s for the CA private key even when certs_dir overlaps config dir", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(certsDir, mitm.CAKeyFileName), []byte("private-key-bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		resp, err := http.Get(srv.URL + "/certs/" + mitm.CAKeyFileName)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 — the CA private key must never be servable via /certs", resp.StatusCode)
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
