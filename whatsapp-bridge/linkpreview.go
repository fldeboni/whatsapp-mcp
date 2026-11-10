package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"golang.org/x/net/html"
	"google.golang.org/protobuf/proto"
)

// Link preview: in WhatsApp the SENDER builds the card. The app reads the page's og:tags and sends title, description
// and thumbnail inside the ExtendedTextMessage; the recipient never fetches the page. Without this the link goes as
// plain text (no card). A small inline JPEG is always sent; a bigger one is uploaded as "link thumbnail" so newer apps
// show the large card. Any failure (site down, no og:tags) just sends the text as before.

var urlRe = regexp.MustCompile(`(?i)\bhttps?://[^\s<>"']+`)

type linkPreview struct {
	matched, title, description string
	image                       []byte
}

const (
	previewUA     = "WhatsApp/2.2445.6 W" // many sites only serve og:tags to known link-preview clients
	browserUA     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"
	maxPageBytes  = 3 << 20
	maxImageBytes = 8 << 20
	inlineThumbPx = 192
	uploadThumbPx = 1024
)

func firstURL(text string) string {
	u := urlRe.FindString(text)
	return strings.TrimRight(u, ".,;:!?)]}")
}

func httpGet(raw, ua string, limit int64) ([]byte, *url.URL, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, nil, "", err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept-Language", "pt-BR,pt;q=0.9,en;q=0.8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	return b, resp.Request.URL, resp.Header.Get("Content-Type"), err
}

// parseOG reads og:/twitter: meta tags, falling back to <title> and meta description.
func parseOG(page []byte) map[string]string {
	out := map[string]string{}
	z := html.NewTokenizer(bytes.NewReader(page))
	inTitle := false
	for {
		switch z.Next() {
		case html.ErrorToken:
			return out
		case html.StartTagToken, html.SelfClosingTagToken:
			t := z.Token()
			switch t.Data {
			case "meta":
				var key, content string
				for _, a := range t.Attr {
					switch strings.ToLower(a.Key) {
					case "property", "name":
						key = strings.ToLower(strings.TrimSpace(a.Val))
					case "content":
						content = strings.TrimSpace(a.Val)
					}
				}
				if key != "" && content != "" {
					if _, seen := out[key]; !seen {
						out[key] = content
					}
				}
			case "title":
				inTitle = true
			case "body":
				return out // tags we want are in <head>
			}
		case html.TextToken:
			if inTitle {
				if _, seen := out["<title>"]; !seen {
					out["<title>"] = strings.TrimSpace(string(z.Text()))
				}
				inTitle = false
			}
		case html.EndTagToken:
			inTitle = false
		}
	}
}

func pick(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := m[k]; v != "" {
			return v
		}
	}
	return ""
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func fetchLinkPreview(text string) *linkPreview {
	raw := firstURL(text)
	if raw == "" {
		return nil
	}
	var og map[string]string
	var base *url.URL
	for _, ua := range []string{previewUA, browserUA} {
		page, final, ctype, err := httpGet(raw, ua, maxPageBytes)
		if err != nil || (ctype != "" && !strings.Contains(ctype, "html")) {
			continue
		}
		og, base = parseOG(page), final
		if pick(og, "og:title", "twitter:title") != "" {
			break
		}
	}
	if og == nil {
		return nil
	}
	p := &linkPreview{
		matched:     raw,
		title:       clip(pick(og, "og:title", "twitter:title", "<title>"), 200),
		description: clip(pick(og, "og:description", "twitter:description", "description"), 300),
	}
	if p.title == "" {
		return nil
	}
	if img := pick(og, "og:image:secure_url", "og:image", "og:image:url", "twitter:image", "twitter:image:src"); img != "" {
		if u, err := base.Parse(img); err == nil {
			if b, _, _, err := httpGet(u.String(), browserUA, maxImageBytes); err == nil && len(b) > 0 {
				p.image = b
			}
		}
	}
	return p
}

// jpegFit re-encodes any image (jpg/png/webp/gif/avif...) as JPEG with the long side <= px. Returns data, w, h.
func jpegFit(img []byte, px int) ([]byte, uint32, uint32) {
	ffmpeg := findFFTool("ffmpeg")
	if ffmpeg == "" || len(img) == 0 {
		return nil, 0, 0
	}
	cmd := exec.Command(ffmpeg, "-v", "error", "-i", "pipe:0", "-frames:v", "1",
		"-vf", fmt.Sprintf("scale='min(%d,iw)':'min(%d,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2", px, px),
		"-q:v", "4", "-f", "mjpeg", "pipe:1")
	cmd.Stdin = bytes.NewReader(img)
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return nil, 0, 0
	}
	w, h := jpegSize(out)
	return out, w, h
}

// jpegSize reads width/height from the JPEG SOF marker.
func jpegSize(b []byte) (uint32, uint32) {
	for i := 2; i+9 < len(b); {
		if b[i] != 0xFF {
			i++
			continue
		}
		marker := b[i+1]
		if marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC {
			return uint32(b[i+7])<<8 | uint32(b[i+8]), uint32(b[i+5])<<8 | uint32(b[i+6])
		}
		i += 2 + int(b[i+2])<<8 + int(b[i+3])
	}
	return 0, 0
}

// applyLinkPreview fills the card fields of an ExtendedTextMessage. Returns false when there is no preview.
func applyLinkPreview(client *whatsmeow.Client, ext *waProto.ExtendedTextMessage, text string) bool {
	p := fetchLinkPreview(text)
	if p == nil {
		return false
	}
	ext.MatchedText = proto.String(p.matched)
	ext.Title = proto.String(p.title)
	if p.description != "" {
		ext.Description = proto.String(p.description)
	}
	ext.PreviewType = waProto.ExtendedTextMessage_NONE.Enum()
	if p.image == nil {
		return true
	}
	if small, _, _ := jpegFit(p.image, inlineThumbPx); small != nil {
		ext.JPEGThumbnail = small
	}
	if big, w, h := jpegFit(p.image, uploadThumbPx); big != nil && w >= 300 {
		if up, err := client.Upload(context.Background(), big, whatsmeow.MediaLinkThumbnail); err == nil {
			ext.ThumbnailDirectPath = proto.String(up.DirectPath)
			ext.ThumbnailSHA256 = up.FileSHA256
			ext.ThumbnailEncSHA256 = up.FileEncSHA256
			ext.MediaKey = up.MediaKey
			ext.MediaKeyTimestamp = proto.Int64(time.Now().Unix())
			ext.ThumbnailWidth, ext.ThumbnailHeight = proto.Uint32(w), proto.Uint32(h)
		}
	}
	return true
}
