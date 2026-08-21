package webui

import (
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/derekhud/dynamic-pac-proxy/internal/config"
	"github.com/derekhud/dynamic-pac-proxy/internal/mitm"
)

// certMimeTypes maps certificate file extensions to the content type that
// makes mobile OSes recognize and offer to install them directly, rather
// than just downloading a generic file. Extensions not listed here are
// served as an attachment instead (see CertsFileHandler) so browsers don't
// render them inline as text.
var certMimeTypes = map[string]string{
	".pem":          "application/x-x509-ca-cert",
	".crt":          "application/x-x509-ca-cert",
	".cer":          "application/x-x509-ca-cert",
	".der":          "application/pkix-cert",
	".mobileconfig": "application/x-apple-aspen-config",
	".p12":          "application/x-pkcs12",
	".pfx":          "application/x-pkcs12",
}

// certsIndexTemplate renders the /certs landing page: a list of whatever
// certificate files are currently sitting in the certs directory, plus
// generic per-platform install instructions. The actual certificate (e.g.
// Charles's root CA) is never bundled with this binary — the user exports
// it themselves and drops it in the certs directory; see certs/README.md.
var certsIndexTemplate = template.Must(template.New("certs").Parse(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>SSL Certificates</title>
<style>
body { font-family: system-ui, sans-serif; max-width: 640px; margin: 2rem auto; padding: 0 1rem; line-height: 1.5; }
h1 { font-size: 1.4rem; }
ul.files { list-style: none; padding: 0; }
ul.files li { margin: 0.5rem 0; }
ul.files a { font-size: 1.05rem; text-decoration: none; border: 1px solid #ccc; border-radius: 6px; padding: 0.5rem 0.8rem; display: inline-block; }
ul.files a:hover { background: #f5f5f5; }
.size { color: #888; font-size: 0.85rem; margin-left: 0.5rem; }
.empty { color: #888; }
details { margin: 1rem 0; }
summary { cursor: pointer; font-weight: 600; }
code { background: #f5f5f5; padding: 0.1rem 0.3rem; border-radius: 3px; }
</style>
</head>
<body>
<h1>SSL Certificates</h1>
<p>Install one of these certificates to let your device trust Charles's
man-in-the-middle SSL proxying for HTTPS sites.</p>

{{if .Files}}
<ul class="files">
{{range .Files}}<li><a href="/certs/{{.Name}}" download>{{.Name}}</a><span class="size">{{.Size}}</span></li>
{{end}}
</ul>
{{else}}
<p class="empty">No certificate files found yet in the certs directory
({{.Dir}}). See <code>certs/README.md</code> for how to export Charles's
root certificate and drop it in.</p>
{{end}}

<details>
<summary>iOS</summary>
<p>Tap the certificate above to download it, then go to
<strong>Settings &gt; General &gt; VPN &amp; Device Management</strong> and
install the downloaded profile. Afterwards, go to
<strong>Settings &gt; General &gt; About &gt; Certificate Trust Settings</strong>
and enable full trust for the certificate.</p>
</details>

<details>
<summary>Android</summary>
<p>Tap the certificate above to download it, then install it from
<strong>Settings &gt; Security &gt; Encryption &amp; credentials &gt; Install a certificate &gt; CA certificate</strong>
(exact path varies by Android version/manufacturer).</p>
</details>

<details>
<summary>macOS</summary>
<p>Download the certificate, double-click it to add it to Keychain Access,
then find it under the <strong>System</strong> keychain, open it, expand
<strong>Trust</strong>, and set "When using this certificate" to
<strong>Always Trust</strong>.</p>
</details>

<details>
<summary>Windows</summary>
<p>Download the certificate, double-click it, choose
<strong>Install Certificate &gt; Local Machine</strong>, and place it in the
<strong>Trusted Root Certification Authorities</strong> store.</p>
</details>

</body>
</html>
`))

type CertFileInfo struct {
	Name string
	Size string
}

// ListCertFiles returns the regular, non-hidden files in dir, sorted by
// name — README.md is excluded since it's documentation for the folder,
// not something to install, and the local CA's private key filename is
// always excluded too: certs_dir and config.Store.ConfigDir (where the CA
// key is written, see internal/mitm) are configured independently, and a
// certs_dir that happens to resolve to the same directory (e.g.
// certs_dir: ".") must never cause the private key to be listed here — it
// would then also be downloadable via CertsFileHandler. A missing
// directory just yields no files; the index page explains what to do
// about that.
func ListCertFiles(dir string) []CertFileInfo {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []CertFileInfo
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.EqualFold(e.Name(), "README.md") || strings.EqualFold(e.Name(), mitm.CAKeyFileName) {
			continue
		}
		info, err := e.Info()
		size := ""
		if err == nil {
			size = formatBytes(info.Size())
		}
		files = append(files, CertFileInfo{Name: e.Name(), Size: size})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files
}

func formatBytes(n int64) string {
	const kb = 1024
	if n < kb {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KB", float64(n)/kb)
}

// CertsIndexHandler serves the /certs landing page listing whatever's
// currently in the certs directory (re-resolved per request so a
// hot-reloaded certs_dir takes effect immediately).
func CertsIndexHandler(cfgStore *config.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dir := cfgStore.CertsDir()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		certsIndexTemplate.Execute(w, struct {
			Files []CertFileInfo
			Dir   string
		}{Files: ListCertFiles(dir), Dir: dir})
	})
}

// CertsFileHandler serves individual files out of the certs directory at
// /certs/<filename>. It sets a content type mobile OSes recognize as an
// installable certificate/profile where possible; unrecognized extensions
// are served as an attachment so browsers download them instead of
// rendering them inline as plain text.
func CertsFileHandler(cfgStore *config.Store) http.Handler {
	return http.StripPrefix("/certs/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path
		if name == "" || strings.Contains(name, "/") || strings.Contains(name, "..") || strings.HasPrefix(name, ".") || strings.EqualFold(name, "README.md") || strings.EqualFold(name, mitm.CAKeyFileName) {
			http.NotFound(w, r)
			return
		}
		dir := cfgStore.CertsDir()
		path := filepath.Join(dir, name)

		if ct, ok := certMimeTypes[strings.ToLower(filepath.Ext(name))]; ok {
			w.Header().Set("Content-Type", ct)
		} else {
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
		}
		http.ServeFile(w, r, path)
	}))
}
