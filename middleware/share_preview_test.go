package middleware

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/cloudreve/Cloudreve/v4/application/constants"
	"github.com/cloudreve/Cloudreve/v4/pkg/filemanager/fs"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func newRequestCtx(target string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, target, nil)
	return c
}

// homeURL builds a properly encoded /home?path=<shareURI> long link.
func homeURL(sharePath string) string {
	q := url.Values{}
	q.Set("path", sharePath)
	return "/home?" + q.Encode()
}

func TestIsSocialMediaBot(t *testing.T) {
	a := assert.New(t)

	bots := []string{
		"facebookexternalhit/1.1",
		"Twitterbot/1.0",
		"Discordbot/2.0 (+https://discordapp.com)",
		"Slackbot-LinkExpanding 1.0",
		"WhatsApp/2.21",
		"LinkedInBot/1.0",
		"TelegramBot (like TwitterBot)",
	}
	for _, ua := range bots {
		a.Truef(isSocialMediaBot(ua), "expected %q to be detected as a bot", ua)
	}

	humans := []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36",
		"curl/8.4.0",
		"",
		"Googlebot/2.1", // search crawler, not a social media preview bot
	}
	for _, ua := range humans {
		a.Falsef(isSocialMediaBot(ua), "expected %q to NOT be detected as a social media bot", ua)
	}
}

func TestExtractShareParams(t *testing.T) {
	cases := []struct {
		name         string
		target       string
		wantID       string
		wantPassword string
		wantSubPath  string
	}{
		// Short links
		{"short root no password", "/s/abc", "abc", "", ""},
		{"short root with password", "/s/abc/secret", "abc", "secret", ""},
		{
			"short deep with password",
			"/s/abc/secret?path=/photos/a.jpg",
			"abc", "secret", "/photos/a.jpg",
		},
		{"short deep no password", "/s/abc?path=sub/deep", "abc", "", "sub/deep"},
		{"short empty id", "/s/", "", "", ""},

		// Long links
		{"long root", homeURL("cloudreve://abc@share"), "abc", "", ""},
		{
			"long deep with password",
			homeURL("cloudreve://abc:secret@share/photos/a.jpg"),
			"abc", "secret", "photos/a.jpg",
		},
		{"long non-share filesystem", homeURL("cloudreve://abc@my"), "", "", ""},
		{"long missing path", "/home", "", "", ""},
		{"long invalid uri", "/home?path=not-a-uri", "", "", ""},

		// Unrelated paths never carry share params.
		{"unrelated path", "/about", "", "", ""},
		{"root path", "/", "", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, password, subPath := extractShareParams(newRequestCtx(tc.target))
			assert.Equal(t, tc.wantID, id, "id")
			assert.Equal(t, tc.wantPassword, password, "password")
			assert.Equal(t, tc.wantSubPath, subPath, "subPath")
		})
	}
}

// TestShareDeepTargetConfinedToShare verifies the security property the preview
// renderer relies on: an attacker-supplied sub-path can never escape the share
// namespace, so illegal traversal paths resolve harmlessly inside the share and
// then fail to load (degrading to the root preview).
func TestShareDeepTargetConfinedToShare(t *testing.T) {
	a := assert.New(t)

	shareURI, err := fs.NewUriFromString(fs.NewShareUri("abc", ""))
	a.NoError(err)

	traversals := []string{
		"../../../../etc/passwd",
		"/../secret",
		"foo/../../bar",
	}
	for _, sub := range traversals {
		target := shareURI.JoinRaw(sub)
		a.Equalf(constants.FileSystemShare, target.FileSystem(),
			"traversal %q must stay in the share filesystem", sub)
		a.NotContainsf(target.PathTrimmed(), "..",
			"traversal %q must be cleaned, got %q", sub, target.PathTrimmed())
	}

	// A normal sub-path round-trips into a well-formed share URI.
	normal := shareURI.JoinRaw("photos/a.jpg")
	a.Equal("photos/a.jpg", normal.PathTrimmed())
	a.Equal("cloudreve://abc@share/photos/a.jpg", normal.String())
}

func TestBuildShareDeepShortURL(t *testing.T) {
	a := assert.New(t)
	base, _ := url.Parse("https://cloud.example.com")

	got := buildShareDeepShortURL(base, "abc", "secret", "photos/a.jpg")

	parsed, err := url.Parse(got)
	a.NoError(err)
	a.Equal("https", parsed.Scheme)
	a.Equal("cloud.example.com", parsed.Host)
	a.Equal("/s/abc/secret", parsed.Path)
	a.Equal("/photos/a.jpg", parsed.Query().Get("path"))
}

func TestBuildShareDeepShortURLNoPassword(t *testing.T) {
	a := assert.New(t)
	base, _ := url.Parse("https://cloud.example.com")

	got := buildShareDeepShortURL(base, "abc", "", "dir/sub")

	parsed, err := url.Parse(got)
	a.NoError(err)
	a.Equal("/s/abc", parsed.Path)
	a.Equal("/dir/sub", parsed.Query().Get("path"))
}

func TestBuildShareDeepRedirectURL(t *testing.T) {
	a := assert.New(t)

	target, err := fs.NewUriFromString("cloudreve://abc:secret@share/photos/a.jpg")
	a.NoError(err)

	got := buildShareDeepRedirectURL(target)

	parsed, err := url.Parse(got)
	a.NoError(err)
	a.Equal("/home", parsed.Path)
	// The redirect must carry the full deep share URI so the SPA opens the target.
	a.Equal("cloudreve://abc:secret@share/photos/a.jpg", parsed.Query().Get("path"))
}

// TestSharePreviewPassThrough ensures normal browsers and non-share bot requests
// flow through to downstream handlers untouched (no OG page, no abort).
func TestSharePreviewPassThrough(t *testing.T) {
	r := gin.New()
	r.Use(SharePreview(nil)) // dep is never touched on the pass-through paths
	r.NoRoute(func(c *gin.Context) { c.String(http.StatusOK, "passthrough") })

	cases := []struct {
		name      string
		target    string
		userAgent string
	}{
		{"normal browser on share link", "/s/abc/secret", "Mozilla/5.0 (X11; Linux x86_64)"},
		{"bot on non-share path", "/about", "Discordbot/2.0"},
		{"normal browser on deep long link", homeURL("cloudreve://abc@share/dir/f.png"), "curl/8.4.0"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			req.Header.Set("User-Agent", tc.userAgent)
			r.ServeHTTP(w, req)

			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, "passthrough", w.Body.String())
			assert.NotContains(t, w.Body.String(), "og:title")
		})
	}
}
