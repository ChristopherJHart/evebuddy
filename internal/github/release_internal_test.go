package github

import (
	"fmt"
	"testing"

	"github.com/jarcoal/httpmock"
	"github.com/stretchr/testify/assert"
)

const latestURL = "https://api.github.com/repos/ErikKalkoken/evebuddy/releases/latest"

// realReleasePayload mirrors the shape of a real response from the Github API,
// trimmed to the fields we read.
func realReleasePayload() map[string]any {
	return map[string]any{
		"tag_name": "v0.75.0",
		"assets": []map[string]any{
			{
				"name":                 "evebuddy-0.75.0-windows-x64.zip",
				"size":                 29881649,
				"digest":               "sha256:de4e52fc635b66fe350e71ce1f9755aff3b5073524e290c743eefc5b6c10216a",
				"browser_download_url": "https://github.com/ErikKalkoken/evebuddy/releases/download/v0.75.0/evebuddy-0.75.0-windows-x64.zip",
				"content_type":         "application/zip",
			},
			{
				"name":                 "EVE_Buddy.apk",
				"size":                 272167348,
				"digest":               "sha256:aaaa",
				"browser_download_url": "https://github.com/ErikKalkoken/evebuddy/releases/download/v0.75.0/EVE_Buddy.apk",
			},
		},
	}
}

func TestFetchGitHubLatestRelease(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	t.Run("should parse assets with digest", func(t *testing.T) {
		httpmock.Reset()
		httpmock.RegisterResponder("GET", latestURL,
			httpmock.NewJsonResponderOrPanic(200, realReleasePayload()))
		r, err := fetchGitHubLatestRelease(t.Context(), "ErikKalkoken", "evebuddy")
		if assert.NoError(t, err) {
			assert.Equal(t, "v0.75.0", r.TagName)
			assert.Equal(t, "0.75.0", r.Version, "the version must be normalized without the leading v")
			assert.Len(t, r.Assets, 2)
			a, ok := r.Asset("evebuddy-0.75.0-windows-x64.zip")
			if assert.True(t, ok) {
				assert.Equal(t, "de4e52fc635b66fe350e71ce1f9755aff3b5073524e290c743eefc5b6c10216a", a.SHA256)
				assert.Equal(t, int64(29881649), a.Size)
				assert.Contains(t, a.DownloadURL, "releases/download/v0.75.0/")
			}
		}
	})
	t.Run("should report missing asset", func(t *testing.T) {
		httpmock.Reset()
		httpmock.RegisterResponder("GET", latestURL,
			httpmock.NewJsonResponderOrPanic(200, realReleasePayload()))
		r, err := fetchGitHubLatestRelease(t.Context(), "ErikKalkoken", "evebuddy")
		if assert.NoError(t, err) {
			_, ok := r.Asset("evebuddy-0.75.0-freebsd-amd64.zip")
			assert.False(t, ok)
		}
	})
	t.Run("should tolerate a release without assets", func(t *testing.T) {
		httpmock.Reset()
		httpmock.RegisterResponder("GET", latestURL,
			httpmock.NewJsonResponderOrPanic(200, map[string]any{"tag_name": "v1.0.0"}))
		r, err := fetchGitHubLatestRelease(t.Context(), "ErikKalkoken", "evebuddy")
		if assert.NoError(t, err) {
			assert.Equal(t, "1.0.0", r.Version)
			assert.Empty(t, r.Assets)
		}
	})
	t.Run("should leave digest empty when absent", func(t *testing.T) {
		httpmock.Reset()
		httpmock.RegisterResponder("GET", latestURL,
			httpmock.NewJsonResponderOrPanic(200, map[string]any{
				"tag_name": "v1.0.0",
				"assets": []map[string]any{
					{"name": "old-asset.zip", "browser_download_url": "https://example.com/old-asset.zip"},
				},
			}))
		r, err := fetchGitHubLatestRelease(t.Context(), "ErikKalkoken", "evebuddy")
		if assert.NoError(t, err) {
			a, ok := r.Asset("old-asset.zip")
			if assert.True(t, ok) {
				assert.Empty(t, a.SHA256)
			}
		}
	})
	t.Run("should report error for an invalid tag", func(t *testing.T) {
		httpmock.Reset()
		httpmock.RegisterResponder("GET", latestURL,
			httpmock.NewJsonResponderOrPanic(200, map[string]any{"tag_name": "not-a-version"}))
		_, err := fetchGitHubLatestRelease(t.Context(), "ErikKalkoken", "evebuddy")
		assert.Error(t, err)
	})
	t.Run("should report error when request failed", func(t *testing.T) {
		httpmock.Reset()
		httpmock.RegisterResponder("GET", latestURL,
			httpmock.NewErrorResponder(fmt.Errorf("some error")))
		_, err := fetchGitHubLatestRelease(t.Context(), "ErikKalkoken", "evebuddy")
		assert.Error(t, err)
	})
	t.Run("should report error when release not found", func(t *testing.T) {
		httpmock.Reset()
		httpmock.RegisterResponder("GET", latestURL,
			httpmock.NewJsonResponderOrPanic(404, map[string]any{"message": "Not found"}))
		_, err := fetchGitHubLatestRelease(t.Context(), "ErikKalkoken", "evebuddy")
		assert.ErrorIs(t, err, ErrHTTPError)
	})
	t.Run("should report error on invalid json", func(t *testing.T) {
		httpmock.Reset()
		httpmock.RegisterResponder("GET", latestURL,
			httpmock.NewStringResponder(200, "invalid"))
		_, err := fetchGitHubLatestRelease(t.Context(), "ErikKalkoken", "evebuddy")
		assert.Error(t, err)
	})
}

func TestParseSHA256Digest(t *testing.T) {
	cases := []struct{ in, want string }{
		{"sha256:abc123", "abc123"},
		{"", ""},
		{"md5:abc123", ""},
		{"abc123", ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, parseSHA256Digest(tc.in), "input %q", tc.in)
	}
}
