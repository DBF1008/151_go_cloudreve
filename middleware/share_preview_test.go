package middleware

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// newTestContext creates a gin.Context from the given request path and optional query.
func newTestContext(path string, query map[string]string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	u := path
	if len(query) > 0 {
		parts := make([]string, 0, len(query))
		for k, v := range query {
			parts = append(parts, k+"="+v)
		}
		u += "?" + strings.Join(parts, "&")
	}

	req, _ := http.NewRequest("GET", u, nil)
	c.Request = req
	return c, w
}

// ---------------------------------------------------------------------------
// isSocialMediaBot
// ---------------------------------------------------------------------------

func TestIsSocialMediaBot(t *testing.T) {
	tests := []struct {
		ua   string
		want bool
	}{
		// Known bots
		{"facebookexternalhit/1.1", true},
		{"FacebookCatalog/1.0", true},
		{"Facebot", true},
		{"Twitterbot/1.0", true},
		{"LinkedInBot/1.0", true},
		{"Discordbot/2.0", true},
		{"TelegramBot (like TwitterBot)", true},
		{"Slackbot-LinkExpanding 1.0", true},
		{"WhatsApp/2.19.81", true},

		// Case insensitivity
		{"FACEBOOKEXTERNALHIT", true},
		{"discordBot", true},
		{"WHATSAPP", true},

		// Non-bots
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64)", false},
		{"Mozilla/5.0 (compatible; Googlebot/2.1)", false},
		{"", false},
		{"curl/7.88.1", false},
	}

	for _, tt := range tests {
		t.Run(tt.ua, func(t *testing.T) {
			assert.Equal(t, tt.want, isSocialMediaBot(tt.ua))
		})
	}
}

// ---------------------------------------------------------------------------
// extractShareParams
// ---------------------------------------------------------------------------

func TestExtractShareParams(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		query    map[string]string
		wantID   string
		wantPwd  string
		wantSub  string
	}{
		// Short links
		{
			name:    "short link without password",
			path:    "/s/abc123",
			wantID:  "abc123",
			wantPwd: "",
			wantSub: "",
		},
		{
			name:    "short link with password",
			path:    "/s/abc123/secret",
			wantID:  "abc123",
			wantPwd: "secret",
			wantSub: "",
		},
		{
			name:    "short link with sub-path query",
			path:    "/s/abc123/secret",
			query:   map[string]string{"path": "photos/vacation"},
			wantID:  "abc123",
			wantPwd: "secret",
			wantSub: "photos/vacation",
		},
		{
			name:    "short link with sub-path without password",
			path:    "/s/abc123",
			query:   map[string]string{"path": "docs/readme.md"},
			wantID:  "abc123",
			wantPwd: "",
			wantSub: "docs/readme.md",
		},
		{
			name:    "short link with empty path query",
			path:    "/s/abc123",
			query:   map[string]string{"path": ""},
			wantID:  "abc123",
			wantPwd: "",
			wantSub: "",
		},
		{
			name:    "short link with traversal in path query",
			path:    "/s/abc123",
			query:   map[string]string{"path": "../../../etc/passwd"},
			wantID:  "abc123",
			wantPwd: "",
			wantSub: "",
		},
		{
			name:    "short link with leading slash in path",
			path:    "/s/abc123",
			query:   map[string]string{"path": "/photos/vacation"},
			wantID:  "abc123",
			wantPwd: "",
			wantSub: "photos/vacation",
		},

		// Long links
		{
			name:    "long link without sub-path",
			path:    "/home",
			query:   map[string]string{"path": "cloudreve://abc123:secret@share"},
			wantID:  "abc123",
			wantPwd: "secret",
			wantSub: "",
		},
		{
			name:    "long link with sub-path",
			path:    "/home",
			query:   map[string]string{"path": "cloudreve://abc123:secret@share/photos/vacation"},
			wantID:  "abc123",
			wantPwd: "secret",
			wantSub: "photos/vacation",
		},
		{
			name:    "long link with deep sub-path",
			path:    "/home",
			query:   map[string]string{"path": "cloudreve://abc123@share/a/b/c/d.txt"},
			wantID:  "abc123",
			wantPwd: "",
			wantSub: "a/b/c/d.txt",
		},
		{
			name:    "long link with trailing slash",
			path:    "/home/",
			query:   map[string]string{"path": "cloudreve://abc123@share/photos/"},
			wantID:  "abc123",
			wantPwd: "",
			wantSub: "photos",
		},

		// Non-share paths
		{
			name:    "non-share URI",
			path:    "/home",
			query:   map[string]string{"path": "cloudreve://abc123@my"},
			wantID:  "",
			wantPwd: "",
			wantSub: "",
		},
		{
			name:    "invalid URI",
			path:    "/home",
			query:   map[string]string{"path": "not-a-uri"},
			wantID:  "",
			wantPwd: "",
			wantSub: "",
		},
		{
			name:    "unrelated path",
			path:    "/api/v4/share",
			wantID:  "",
			wantPwd: "",
			wantSub: "",
		},
		{
			name:    "root path",
			path:    "/",
			wantID:  "",
			wantPwd: "",
			wantSub: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestContext(tt.path, tt.query)
			id, pwd, sub := extractShareParams(c)
			assert.Equal(t, tt.wantID, id, "id mismatch")
			assert.Equal(t, tt.wantPwd, pwd, "password mismatch")
			assert.Equal(t, tt.wantSub, sub, "subPath mismatch")
		})
	}
}

// ---------------------------------------------------------------------------
// sanitizeSubPath
// ---------------------------------------------------------------------------

func TestSanitizeSubPath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// Valid paths
		{"simple file", "readme.md", "readme.md"},
		{"nested folder", "photos/vacation", "photos/vacation"},
		{"deep path", "a/b/c/d/e.txt", "a/b/c/d/e.txt"},
		{"single folder", "docs", "docs"},

		// Leading/trailing slashes stripped
		{"leading slash", "/photos/vacation", "photos/vacation"},
		{"trailing slash", "photos/vacation/", "photos/vacation"},
		{"both slashes", "/photos/vacation/", "photos/vacation"},

		// Empty or dot-only
		{"empty string", "", ""},
		{"whitespace only", "   ", ""},
		{"single dot", ".", ""},
		{"slash only", "/", ""},
		{"double slash", "//", ""},

		// Path traversal — always rejected
		{"dotdot prefix", "../../../etc/passwd", ""},
		{"dotdot in middle", "photos/../secret", ""},
		{"dotdot suffix", "photos/..", ""},
		{"dotdot only", "..", ""},
		{"dotdot with slash", "/../etc", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, sanitizeSubPath(tt.in))
		})
	}
}

// ---------------------------------------------------------------------------
// renderOGHTML
// ---------------------------------------------------------------------------

func TestRenderOGHTML(t *testing.T) {
	data := &ogData{
		SiteName:    "MyCloud",
		Title:       "vacation.jpg",
		Description: "2.50 MB · Alice",
		ImageURL:    "https://cdn.example.com/thumb.jpg",
		ShareURL:    "https://cloud.example.com/s/abc123?path=photos",
		RedirectURL: "/home?path=cloudreve%3A%2F%2Fabc123%40share%2Fphotos",
	}

	html := renderOGHTML(data)
	assert.Contains(t, html, `<meta property="og:title" content="vacation.jpg">`)
	assert.Contains(t, html, `<meta property="og:description" content="2.50 MB · Alice">`)
	assert.Contains(t, html, `<meta property="og:image" content="https://cdn.example.com/thumb.jpg">`)
	assert.Contains(t, html, `<meta property="og:url" content="https://cloud.example.com/s/abc123?path=photos">`)
	assert.Contains(t, html, `<meta property="og:site_name" content="MyCloud">`)
	assert.Contains(t, html, `<meta name="twitter:card" content="summary">`)
	assert.Contains(t, html, `<meta name="twitter:title" content="vacation.jpg">`)
	assert.Contains(t, html, `<title>vacation.jpg - MyCloud</title>`)
	assert.Contains(t, html, `window.location.href = "/home?path=cloudreve%3A%2F%2Fabc123%40share%2Fphotos"`)
	assert.Contains(t, html, `<noscript><a href="/home?path=cloudreve%3A%2F%2Fabc123%40share%2Fphotos">vacation.jpg</a></noscript>`)
}

func TestRenderOGHTML_EmptyData(t *testing.T) {
	data := &ogData{}
	html := renderOGHTML(data)
	assert.Contains(t, html, `<meta property="og:title" content="">`)
	assert.Contains(t, html, `<meta property="og:description" content="">`)
	assert.NotEmpty(t, html)
}

// ---------------------------------------------------------------------------
// formatFileSize
// ---------------------------------------------------------------------------

func TestFormatFileSize(t *testing.T) {
	tests := []struct {
		size int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.00 KB"},
		{1536, "1.50 KB"},
		{1048576, "1.00 MB"},
		{2621440, "2.50 MB"},
		{1073741824, "1.00 GB"},
		{1099511627776, "1.00 TB"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, tt.want, formatFileSize(tt.size))
		})
	}
}

// ---------------------------------------------------------------------------
// resolveURL
// ---------------------------------------------------------------------------

func TestResolveURL(t *testing.T) {
	tests := []struct {
		name string
		base string
		path string
		want string
	}{
		{
			"absolute http",
			"https://cloud.example.com",
			"http://cdn.example.com/icon.png",
			"http://cdn.example.com/icon.png",
		},
		{
			"absolute https",
			"https://cloud.example.com",
			"https://cdn.example.com/icon.png",
			"https://cdn.example.com/icon.png",
		},
		{
			"relative path",
			"https://cloud.example.com",
			"/static/icon.png",
			"https://cloud.example.com/static/icon.png",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, _ := parseBaseURL(tt.base)
			assert.Equal(t, tt.want, resolveURL(base, tt.path))
		})
	}
}

func parseBaseURL(raw string) (*url.URL, error) {
	return url.Parse(raw)
}

// ---------------------------------------------------------------------------
// buildShareThumbUri
// ---------------------------------------------------------------------------

func TestBuildShareThumbUri(t *testing.T) {
	tests := []struct {
		name     string
		shareID  string
		password string
		subPath  string
		fileName string
		wantSub  string // expected substring in result
		wantNot  string // substring that must NOT appear
	}{
		{
			name:     "root file (no sub-path)",
			shareID:  "abc123",
			password: "secret",
			subPath:  "",
			fileName: "readme.md",
			wantSub:  "readme.md",
		},
		{
			name:     "sub-path file",
			shareID:  "abc123",
			password: "secret",
			subPath:  "photos/vacation.jpg",
			fileName: "readme.md",
			wantSub:  "photos/vacation.jpg",
			wantNot:  "readme.md",
		},
		{
			name:     "sub-path without password",
			shareID:  "abc123",
			password: "",
			subPath:  "docs/guide.pdf",
			fileName: "folder",
			wantSub:  "docs/guide.pdf",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildShareThumbUri(tt.shareID, tt.password, tt.subPath, tt.fileName)
			assert.NotEmpty(t, result)
			assert.Contains(t, result, tt.wantSub)
			if tt.wantNot != "" {
				assert.NotContains(t, result, tt.wantNot)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// buildSubPathRedirectURL
// ---------------------------------------------------------------------------

func TestBuildSubPathRedirectURL(t *testing.T) {
	tests := []struct {
		name     string
		id       string
		password string
		subPath  string
		wantSub  string
	}{
		{
			name:     "with sub-path and password",
			id:       "abc123",
			password: "secret",
			subPath:  "photos/vacation",
			wantSub:  "photos",
		},
		{
			name:     "with sub-path no password",
			id:       "abc123",
			password: "",
			subPath:  "docs/readme.md",
			wantSub:  "docs",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildSubPathRedirectURL(tt.id, tt.password, tt.subPath)
			assert.Contains(t, result, "/home?path=")
			assert.Contains(t, result, tt.wantSub)
			assert.Contains(t, result, tt.id)
		})
	}
}
