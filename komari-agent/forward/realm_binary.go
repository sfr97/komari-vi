package forward

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	pkg_flags "github.com/komari-monitor/komari-agent/cmd/flags"
	"github.com/komari-monitor/komari-agent/dnsresolver"
)

func ensureRealmBinary(downloadURL string, force bool) (string, string, error) {
	candidates := []string{"/usr/local/bin/realm", "/usr/bin/realm"}

	if !force {
		for _, p := range candidates {
			if fileExists(p) {
				version := getRealmVersion(p)
				return p, version, nil
			}
		}
		if path, err := exec.LookPath("realm"); err == nil {
			version := getRealmVersion(path)
			return path, version, nil
		}
	}

	if downloadURL == "" {
		downloadURL = defaultRealmDownloadURL()
	}
	if downloadURL == "" {
		return "", "", fmt.Errorf("realm binary not found and no download url provided")
	}

	target := candidates[0]
	if err := downloadTo(downloadURL, target); err != nil {
		return "", "", err
	}
	if err := os.Chmod(target, 0o755); err != nil {
		return "", "", err
	}
	version := getRealmVersion(target)
	return target, version, nil
}

func mapRealmOS(goos string) string {
	switch strings.ToLower(strings.TrimSpace(goos)) {
	case "windows":
		return "windows"
	case "darwin":
		return "macos"
	case "linux":
		return "linux"
	default:
		return ""
	}
}

func mapRealmArch(goarch string) string {
	switch strings.ToLower(strings.TrimSpace(goarch)) {
	case "amd64":
		return "x86_64"
	case "386":
		return "i686"
	case "arm64":
		return "arm64"
	case "arm":
		return "armv7"
	default:
		return ""
	}
}

func defaultRealmDownloadURL() string {
	flags := pkg_flags.GlobalConfig
	endpoint := strings.TrimSuffix(strings.TrimSpace(flags.Endpoint), "/")
	token := strings.TrimSpace(flags.Token)
	if endpoint == "" || token == "" {
		return ""
	}
	osName := mapRealmOS(runtime.GOOS)
	arch := mapRealmArch(runtime.GOARCH)
	if osName == "" || arch == "" {
		return ""
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	u.Path = "/api/v1/realm/binaries/download"
	q := u.Query()
	q.Set("token", token)
	q.Set("os", osName)
	q.Set("arch", arch)
	u.RawQuery = q.Encode()
	return u.String()
}

func downloadTo(urlStr, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}

	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		return err
	}
	flags := pkg_flags.GlobalConfig
	if flags.CFAccessClientID != "" && flags.CFAccessClientSecret != "" {
		req.Header.Set("CF-Access-Client-Id", flags.CFAccessClientID)
		req.Header.Set("CF-Access-Client-Secret", flags.CFAccessClientSecret)
	}

	client := dnsresolver.GetHTTPClient(60 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download realm failed: %s", resp.Status)
	}
	tmpPath := target + ".tmp"
	tmpFile, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	defer tmpFile.Close()

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		return err
	}
	return os.Rename(tmpPath, target)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func getRealmVersion(path string) string {
	out, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
