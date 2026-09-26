package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/hashicorp/go-version"
)

// ErrAssetNotFound is returned when a release does not contain a requested asset.
var ErrAssetNotFound = fmt.Errorf("release asset not found")

// Asset represents a file attached to a Github release.
type Asset struct {
	DownloadURL string
	Name        string
	SHA256      string // hex encoded, empty when Github reports no digest
	Size        int64
}

// Release represents a release of a repo on Github.
type Release struct {
	Assets  []Asset
	TagName string // as published, e.g. v1.2.3
	Version string // normalized, e.g. 1.2.3
}

// Asset returns the asset with the given name and reports whether it was found.
func (r Release) Asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// LatestRelease returns the latest release of a repo on Github.
func LatestRelease(ctx context.Context, owner, repo string) (Release, error) {
	return fetchGitHubLatestRelease(ctx, owner, repo)
}

type githubAsset struct {
	BrowserDownloadURL string `json:"browser_download_url"`
	Digest             string `json:"digest"`
	Name               string `json:"name"`
	Size               int64  `json:"size"`
}

func fetchGitHubLatestRelease(ctx context.Context, owner, repo string) (Release, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, repo)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return Release{}, err
	}
	client := &http.Client{
		Timeout: timeout,
	}
	r, err := client.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer r.Body.Close()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return Release{}, err
	}
	if r.StatusCode >= 400 {
		return Release{}, fmt.Errorf("%s: %w", r.Status, ErrHTTPError)
	}
	var info struct {
		Assets  []githubAsset `json:"assets"`
		TagName string        `json:"tag_name"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return Release{}, err
	}
	rr := Release{
		TagName: info.TagName,
		Assets:  make([]Asset, 0, len(info.Assets)),
	}
	if info.TagName != "" {
		v, err := version.NewVersion(info.TagName)
		if err != nil {
			return Release{}, fmt.Errorf("parse tag %q: %w", info.TagName, err)
		}
		rr.Version = v.String()
	}
	for _, a := range info.Assets {
		rr.Assets = append(rr.Assets, Asset{
			DownloadURL: a.BrowserDownloadURL,
			Name:        a.Name,
			SHA256:      parseSHA256Digest(a.Digest),
			Size:        a.Size,
		})
	}
	return rr, nil
}

// parseSHA256Digest returns the hex digest from a Github asset digest field,
// e.g. "sha256:abc123...". It returns an empty string for any other format,
// since a missing digest is not an error and only disables verification.
func parseSHA256Digest(s string) string {
	hex, ok := strings.CutPrefix(s, "sha256:")
	if !ok {
		return ""
	}
	return hex
}
