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
	"github.com/cloudreve/Cloudreve/v4/inventory/types"
	"github.com/cloudreve/Cloudreve/v4/pkg/cluster/routes"
	"github.com/cloudreve/Cloudreve/v4/pkg/filemanager/fs"
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

// extractShareParams resolves the share id, password and the (optional) sub-path
// inside the shared directory from the request. Two link shapes are supported:
//
//   - Short link:  /s/<id>/<password>?path=/sub/target  (sub-path in the query)
//   - Long link:   /home?path=cloudreve://<id>:<pw>@share/sub/target
//
// subPath is empty when the request targets the share root.
func extractShareParams(c *gin.Context) (id, password, subPath string) {
	urlPath := c.Request.URL.Path

	if strings.HasPrefix(urlPath, "/s/") {
		parts := strings.Split(strings.TrimPrefix(urlPath, "/s/"), "/")
		if len(parts) >= 1 && parts[0] != "" {
			id = parts[0]
			if len(parts) >= 2 {
				password = parts[1]
			}
			subPath = c.Query("path")
		}
	} else if urlPath == "/home" || urlPath == "/home/" {
		rawPath := c.Query("path")
		uri, err := fs.NewUriFromString(rawPath)
		if err != nil || uri.FileSystem() != constants.FileSystemShare {
			return "", "", ""
		}

		return uri.ID(""), uri.Password(), uri.PathTrimmed()
	}

	return id, password, subPath
}

func renderShareOGPage(c *gin.Context, dep dependency.Dep, id, password, subPath string) string {
	settings := dep.SettingProvider()
	siteBasic := settings.SiteBasic(c)
	pwa := settings.PWA(c)
	base := settings.SiteURL(c)

	data := &ogData{
		SiteName:    siteBasic.Name,
		Title:       siteBasic.Name,
		Description: siteBasic.Description,
		ShareURL:    routes.MasterShareUrl(base, id, password).String(),
		RedirectURL: routes.MasterShareLongUrl(id, password).String(),
	}

	if pwa.LargeIcon != "" {
		data.ImageURL = resolveURL(base, pwa.LargeIcon)
	} else if pwa.MediumIcon != "" {
		data.ImageURL = resolveURL(base, pwa.MediumIcon)
	}

	shareID, err := dep.HashIDEncoder().Decode(id, hashid.ShareID)
	if err != nil {
		data.Description = ogStatusInvalidLink
		return renderOGHTML(data)
	}

	shareInfo, err := loadShareForOG(c, shareID, password)
	if err != nil {
		var appErr serializer.AppError
		if errors.As(err, &appErr) {
			data.Description = appErr.Msg
		} else {
			data.Description = ogStatusInvalidLink
		}
		return renderOGHTML(data)
	}

	// Default to the root share preview. These values also serve as the safe
	// fallback whenever a requested sub-path turns out to be illegal.
	name := shareInfo.Name
	isFolder := shareInfo.SourceType != nil && *shareInfo.SourceType == types.FileTypeFolder
	size := shareInfo.Size
	unlocked := shareInfo.Unlocked

	var thumbURI string
	if shareURI, uriErr := fs.NewUriFromString(fs.NewShareUri(id, password)); uriErr == nil {
		// Root thumbnail lives at <shareRoot>/<name> so single-file shares resolve.
		thumbURI = shareURI.Join(name).String()

		// When a sub-path is requested, describe the deep target instead. The
		// share navigator confines resolution to the share root and enforces the
		// password, so any illegal sub-path simply fails and degrades to root.
		if subPath != "" {
			targetURI := shareURI.JoinRaw(subPath)
			if cleanSub := targetURI.PathTrimmed(); cleanSub != "" {
				if target, ok := loadShareTargetInfo(c, targetURI.String()); ok {
					name = target.Name
					isFolder = target.Type == int(types.FileTypeFolder)
					size = target.Size
					unlocked = true
					thumbURI = targetURI.String()

					data.ShareURL = buildShareDeepShortURL(base, id, password, cleanSub)
					data.RedirectURL = buildShareDeepRedirectURL(targetURI)
				}
			}
		}
	}

	data.Title = name
	if isFolder {
		data.Description = "Folder"
	} else if unlocked {
		data.Description = formatFileSize(size)
		if thumbURI != "" {
			if thumbnail, err := loadShareThumbnail(c, thumbURI); err == nil {
				data.ImageURL = thumbnail
			}
		}
	}

	data.Description += " · " + shareInfo.Owner.Nickname
	return renderOGHTML(data)
}

func loadShareThumbnail(c *gin.Context, thumbURI string) (string, error) {
	subService := &explorer.FileThumbService{Uri: thumbURI}

	if err := SetUserCtx(c, 0); err != nil {
		return "", err
	}

	res, err := subService.Get(c)
	if err != nil {
		return "", err
	}

	return res.Url, nil
}

// loadShareTargetInfo resolves the file/folder addressed by a full share URI
// (including any sub-path). It returns ok=false for any inaccessible target —
// non-existent path, wrong/missing password, or insufficient permission — so the
// caller can safely fall back to the share root preview.
func loadShareTargetInfo(c *gin.Context, targetURI string) (*explorer.FileResponse, bool) {
	if err := SetUserCtx(c, 0); err != nil {
		return nil, false
	}

	subService := &explorer.GetFileInfoService{Uri: targetURI}
	res, err := subService.Get(c)
	if err != nil || res == nil {
		return nil, false
	}

	return res, true
}

// buildShareDeepShortURL builds the canonical short share URL pointing at a deep
// target, e.g. /s/<id>/<password>?path=/sub/target.
func buildShareDeepShortURL(base *url.URL, id, password, subPath string) string {
	u := routes.MasterShareUrl(base, id, password)
	q := u.Query()
	q.Set("path", fs.Separator+subPath)
	u.RawQuery = q.Encode()
	return u.String()
}

// buildShareDeepRedirectURL builds the SPA redirect URL that opens the deep
// target directly (/home?path=cloudreve://<id>:<pw>@share/sub/target).
func buildShareDeepRedirectURL(target *fs.URI) string {
	route, _ := url.Parse("/home")
	q := route.Query()
	q.Set("path", target.String())
	route.RawQuery = q.Encode()
	return route.String()
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
