package middleware

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"net/url"
	"strings"

	"github.com/cloudreve/Cloudreve/v4/application/constants"
	"github.com/cloudreve/Cloudreve/v4/application/dependency"
	"github.com/cloudreve/Cloudreve/v4/inventory"
	"github.com/cloudreve/Cloudreve/v4/inventory/types"
	"github.com/cloudreve/Cloudreve/v4/pkg/cluster/routes"
	"github.com/cloudreve/Cloudreve/v4/pkg/filemanager/fs"
	"github.com/cloudreve/Cloudreve/v4/pkg/filemanager/manager"
	"github.com/cloudreve/Cloudreve/v4/pkg/hashid"
	"github.com/cloudreve/Cloudreve/v4/pkg/serializer"
	"github.com/cloudreve/Cloudreve/v4/pkg/util"
	"github.com/cloudreve/Cloudreve/v4/service/explorer"
	"github.com/cloudreve/Cloudreve/v4/service/share"
	"github.com/gin-gonic/gin"
)

const (
	ogStatusInvalidLink = "Invalid Link"
)

type ogData struct {
	SiteName    string
	Title       string
	Description string
	ImageURL    string
	ShareURL    string
	RedirectURL string
}

const ogHTMLTemplate = `<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <meta property="og:title" content="{{.Title}}">
    <meta property="og:description" content="{{.Description}}">
    <meta property="og:image" content="{{.ImageURL}}">
    <meta property="og:url" content="{{.ShareURL}}">
    <meta property="og:type" content="website">
    <meta property="og:site_name" content="{{.SiteName}}">
    <meta name="twitter:card" content="summary">
    <meta name="twitter:title" content="{{.Title}}">
    <meta name="twitter:description" content="{{.Description}}">
    <meta name="twitter:image" content="{{.ImageURL}}">
    <title>{{.Title}} - {{.SiteName}}</title>
</head>
<body>
    <script>window.location.href = "{{.RedirectURL}}";</script>
    <noscript><a href="{{.RedirectURL}}">{{.Title}}</a></noscript>
</body>
</html>`

var ogTemplate = template.Must(template.New("og").Parse(ogHTMLTemplate))

var socialMediaBots = []string{
	"facebookexternalhit",
	"facebookcatalog",
	"facebot",
	"twitterbot",
	"linkedinbot",
	"discordbot",
	"telegrambot",
	"slackbot",
	"whatsapp",
}

func isSocialMediaBot(ua string) bool {
	ua = strings.ToLower(ua)
	for _, bot := range socialMediaBots {
		if strings.Contains(ua, bot) {
			return true
		}
	}
	return false
}

// SharePreview 为社交媒体爬虫渲染OG预览页面
func SharePreview(dep dependency.Dep) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !isSocialMediaBot(c.GetHeader("User-Agent")) {
			c.Next()
			return
		}

		id, password, subPath := extractShareParams(c)
		if id == "" {
			c.Next()
			return
		}

		html := renderShareOGPage(c, dep, id, password, subPath)
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.Header("Cache-Control", "public, no-cache")
		c.String(200, html)
		c.Abort()
	}
}

func extractShareParams(c *gin.Context) (id, password, subPath string) {
	urlPath := c.Request.URL.Path

	if strings.HasPrefix(urlPath, "/s/") {
		parts := strings.Split(strings.TrimPrefix(urlPath, "/s/"), "/")
		if len(parts) >= 1 && parts[0] != "" {
			id = parts[0]
			if len(parts) >= 2 {
				password = parts[1]
			}
		}
		// Short links carry sub-path in the ?path= query parameter
		subPath = sanitizeSubPath(c.Query("path"))
	} else if urlPath == "/home" || urlPath == "/home/" {
		rawPath := c.Query("path")
		uri, err := fs.NewUriFromString(rawPath)
		if err != nil || uri.FileSystem() != constants.FileSystemShare {
			return "", "", ""
		}

		return uri.ID(""), uri.Password(), sanitizeSubPath(uri.PathTrimmed())
	}

	return id, password, subPath
}

// sanitizeSubPath cleans a user-supplied sub-path for safe use in share URI construction.
// It strips path traversal attempts (..), empty segments, and leading/trailing slashes.
// Returns empty string if the path contains unsafe components.
func sanitizeSubPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}

	// Reject raw ".." components before cleaning to prevent path traversal.
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return ""
		}
	}

	cleaned := strings.TrimPrefix(p, "/")
	cleaned = strings.TrimSuffix(cleaned, "/")
	if cleaned == "" || cleaned == "." {
		return ""
	}

	return cleaned
}

func renderShareOGPage(c *gin.Context, dep dependency.Dep, id, password, subPath string) string {
	settings := dep.SettingProvider()
	siteBasic := settings.SiteBasic(c)
	pwa := settings.PWA(c)
	base := settings.SiteURL(c)

	// Build default share URL and redirect URL (root share, no sub-path).
	shareURL := routes.MasterShareUrl(base, id, password).String()
	redirectURL := routes.MasterShareLongUrl(id, password).String()
	if subPath != "" {
		redirectURL = buildSubPathRedirectURL(id, password, subPath)
	}

	data := &ogData{
		SiteName:    siteBasic.Name,
		Title:       siteBasic.Name,
		Description: siteBasic.Description,
		ShareURL:    shareURL,
		RedirectURL: redirectURL,
	}

	if pwa.LargeIcon != "" {
		data.ImageURL = resolveURL(base, pwa.LargeIcon)
	} else if pwa.MediumIcon != "" {
		data.ImageURL = resolveURL(base, pwa.MediumIcon)
	}

	shareIDInt, err := dep.HashIDEncoder().Decode(id, hashid.ShareID)
	if err != nil {
		data.Description = ogStatusInvalidLink
		return renderOGHTML(data)
	}

	shareInfo, err := loadShareForOG(c, shareIDInt, password)
	if err != nil {
		var appErr serializer.AppError
		if errors.As(err, &appErr) {
			data.Description = appErr.Msg
		} else {
			data.Description = ogStatusInvalidLink
		}
		return renderOGHTML(data)
	}

	// Start with root share metadata.
	data.Title = shareInfo.Name
	targetName := shareInfo.Name
	targetSize := shareInfo.Size
	var targetType *types.FileType
	if shareInfo.SourceType != nil {
		t := *shareInfo.SourceType
		targetType = &t
	}

	// If the share is unlocked and a valid sub-path is provided, resolve it to
	// produce OGP metadata that reflects the actual target file or folder.
	// On any failure (invalid path, permission denied, etc.) we silently fall
	// back to the root share metadata — this is the "safe degradation" path.
	if shareInfo.Unlocked && subPath != "" {
		if file, resolveErr := resolveSubPath(c, dep, id, password, subPath); resolveErr == nil && file != nil {
			targetName = file.DisplayName()
			t := file.Type()
			targetType = &t
			targetSize = file.Size()

			// Override share URL to point at the resolved sub-path target.
			subURL := routes.MasterShareUrl(base, id, password)
			q := subURL.Query()
			q.Set("path", subPath)
			subURL.RawQuery = q.Encode()
			data.ShareURL = subURL.String()
		}
		// If resolveErr != nil we keep root share metadata — safe degradation.
	}

	// Populate description and thumbnail based on resolved target type.
	if targetType != nil && *targetType == types.FileTypeFolder {
		data.Description = "Folder"
	} else if shareInfo.Unlocked {
		data.Description = formatFileSize(targetSize)
		if targetType != nil && *targetType == types.FileTypeFile {
			thumbURI := buildShareThumbUri(id, password, subPath, targetName)
			if thumbURL, err := loadShareThumbnail(c, thumbURI); err == nil {
				data.ImageURL = thumbURL
			}
		}
	}

	data.Title = targetName
	data.Description += " · " + shareInfo.Owner.Nickname
	return renderOGHTML(data)
}

// resolveSubPath resolves a sub-path within a share to the target file or folder.
// Returns nil, nil if the path is empty. Returns nil, error on any resolution failure.
func resolveSubPath(c *gin.Context, dep dependency.Dep, shareID, password, subPath string) (fs.File, error) {
	if subPath == "" {
		return nil, nil
	}

	shareUri, err := fs.NewUriFromString(fs.NewShareUri(shareID, password))
	if err != nil {
		return nil, fmt.Errorf("failed to construct share uri: %w", err)
	}
	targetUri := shareUri.JoinRaw(subPath)

	if err := SetUserCtx(c, 0); err != nil {
		return nil, err
	}

	u := inventory.UserFromContext(c)
	m := manager.NewFileManager(dep, u)
	defer m.Recycle()

	file, err := m.Get(c, targetUri)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve sub-path: %w", err)
	}

	return file, nil
}

// buildSubPathRedirectURL constructs the long redirect URL that includes a sub-path.
func buildSubPathRedirectURL(id, password, subPath string) string {
	base, _ := url.Parse("/home")
	q := base.Query()
	shareUri, err := fs.NewUriFromString(fs.NewShareUri(id, password))
	if err != nil {
		return routes.MasterShareLongUrl(id, password).String()
	}
	q.Set("path", shareUri.JoinRaw(subPath).String())
	base.RawQuery = q.Encode()
	return base.String()
}

// buildShareThumbUri constructs the full share URI used for thumbnail loading.
// If subPath is non-empty the URI targets the sub-path file; otherwise it targets
// the root share file identified by fileName.
func buildShareThumbUri(shareID, password, subPath, fileName string) string {
	shareUri, _ := fs.NewUriFromString(fs.NewShareUri(shareID, password))
	if shareUri == nil {
		return ""
	}
	if subPath != "" {
		return shareUri.JoinRaw(subPath).String()
	}
	return shareUri.Join(fileName).String()
}

// loadShareThumbnail loads the thumbnail URL for the file identified by the given
// share URI. The URI should already point to the target file (root or sub-path).
func loadShareThumbnail(c *gin.Context, shareUriStr string) (string, error) {
	if shareUriStr == "" {
		return "", fmt.Errorf("empty share uri")
	}

	subService := &explorer.FileThumbService{
		Uri: shareUriStr,
	}

	if err := SetUserCtx(c, 0); err != nil {
		return "", err
	}

	res, err := subService.Get(c)
	if err != nil {
		return "", err
	}

	return res.Url, nil
}

func loadShareForOG(c *gin.Context, shareID int, password string) (*explorer.Share, error) {
	subService := &share.ShareInfoService{
		Password:   password,
		CountViews: false,
	}

	if err := SetUserCtx(c, 0); err != nil {
		return nil, err
	}

	util.WithValue(c, hashid.ObjectIDCtx{}, shareID)
	return subService.Get(c)
}

func renderOGHTML(data *ogData) string {
	var buf bytes.Buffer
	if err := ogTemplate.Execute(&buf, data); err != nil {
		return ""
	}
	return buf.String()
}

func resolveURL(base *url.URL, path string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	return base.ResolveReference(&url.URL{Path: path}).String()
}

func formatFileSize(size int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
		TB = 1024 * GB
	)

	switch {
	case size >= TB:
		return fmt.Sprintf("%.2f TB", float64(size)/TB)
	case size >= GB:
		return fmt.Sprintf("%.2f GB", float64(size)/GB)
	case size >= MB:
		return fmt.Sprintf("%.2f MB", float64(size)/MB)
	case size >= KB:
		return fmt.Sprintf("%.2f KB", float64(size)/KB)
	default:
		return fmt.Sprintf("%d B", size)
	}
}
