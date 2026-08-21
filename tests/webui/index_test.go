package webui_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/derekhud/dynamic-pac-proxy/internal/config"
	"github.com/derekhud/dynamic-pac-proxy/internal/webui"
)

func TestIndexHandlerListsPACURLs(t *testing.T) {
	cfgStore := config.NewStore(config.FileConfig{
		ListenAddr:    ":8080",
		AdvertiseHost: "172.16.2.22",
		Hosts: []config.HostConfig{
			{Name: "derek-macbook", HostName: "dereks-MacBook-Pro.local", HostPort: 8888, ServerPort: 8081},
			{Name: "mitmproxy-box", HostIP: "172.16.2.23", HostPort: 8080, ServerPort: 8083, InterceptSSL: true},
		},
	}, "/opt/dynamic-pac-proxy/config.yaml")

	srv := httptest.NewServer(webui.IndexHandler(cfgStore))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(bodyBytes)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for _, want := range []string{
		"http://172.16.2.22:8080/proxy/derek-macbook.pac",
		"http://172.16.2.22:8080/proxy/mitmproxy-box.pac",
		"172.16.2.22:8081", // derek-macbook's manual proxy address
		"172.16.2.22:8083", // mitmproxy-box's manual proxy address
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected index page to contain %q, got:\n%s", want, body)
		}
	}
}

func TestIndexHandlerOnlyMatchesRoot(t *testing.T) {
	cfgStore := config.NewStore(config.FileConfig{ListenAddr: ":8080"}, "/opt/dynamic-pac-proxy/config.yaml")
	srv := httptest.NewServer(webui.IndexHandler(cfgStore))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/something-else")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a non-root path", resp.StatusCode)
	}
}

func TestIndexHandlerWithoutAdvertiseHost(t *testing.T) {
	cfgStore := config.NewStore(config.FileConfig{
		ListenAddr: ":8080",
		Hosts: []config.HostConfig{
			{Name: "derek-macbook", HostName: "dereks-MacBook-Pro.local", HostPort: 8888, ServerPort: 8081},
		},
	}, "/opt/dynamic-pac-proxy/config.yaml")

	srv := httptest.NewServer(webui.IndexHandler(cfgStore))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)
	body := string(bodyBytes)

	if strings.Contains(body, "http://:8080") {
		t.Fatalf("expected no broken URL when advertise_host is unset, got:\n%s", body)
	}
	if !strings.Contains(body, "/proxy/derek-macbook.pac") {
		t.Fatalf("expected a relative PAC link even without advertise_host, got:\n%s", body)
	}
}
