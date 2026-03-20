package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// WebTool supports fetch operations.
// Args: {"url": "https://..."}

// Maximum response size to return to the LLM (in characters).
const maxWebResponseChars = 16000

type WebTool struct{}

func NewWebTool() *WebTool { return &WebTool{} }

func (t *WebTool) Name() string        { return "web" }
func (t *WebTool) Description() string { return "Fetch web content from a URL and extract readable text" }

func (t *WebTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"url": map[string]interface{}{
				"type":        "string",
				"description": "The URL to fetch (must be http or https)",
			},
		},
		"required": []string{"url"},
	}
}

func (t *WebTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	u, ok := args["url"].(string)
	if !ok || u == "" {
		return "", fmt.Errorf("web: 'url' argument required")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Picobot/1.0)")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Limit read to 2MB to avoid memory issues
	limited := io.LimitReader(resp.Body, 2*1024*1024)
	b, err := io.ReadAll(limited)
	if err != nil {
		return "", err
	}

	raw := string(b)

	// If it looks like HTML, extract text content
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "html") || strings.HasPrefix(strings.TrimSpace(raw), "<") {
		raw = extractText(raw)
	}

	// Truncate to keep context window manageable
	if len(raw) > maxWebResponseChars {
		raw = raw[:maxWebResponseChars] + "\n\n[... truncated, showing first 16000 chars ...]"
	}

	return fmt.Sprintf("URL: %s\nStatus: %d\n\n%s", u, resp.StatusCode, raw), nil
}

// extractText strips HTML to readable text.
func extractText(html string) string {
	// Remove script and style blocks entirely
	reScript := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	html = reScript.ReplaceAllString(html, "")
	reStyle := regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	html = reStyle.ReplaceAllString(html, "")
	// Remove SVG blocks
	reSVG := regexp.MustCompile(`(?is)<svg[^>]*>.*?</svg>`)
	html = reSVG.ReplaceAllString(html, "")
	// Remove HTML comments
	reComment := regexp.MustCompile(`(?s)<!--.*?-->`)
	html = reComment.ReplaceAllString(html, "")

	// Replace block-level tags with newlines
	reBlock := regexp.MustCompile(`(?i)</(p|div|h[1-6]|li|tr|br|hr)[^>]*>`)
	html = reBlock.ReplaceAllString(html, "\n")
	reBR := regexp.MustCompile(`(?i)<br\s*/?>`)
	html = reBR.ReplaceAllString(html, "\n")

	// Strip all remaining HTML tags
	reTags := regexp.MustCompile(`<[^>]+>`)
	html = reTags.ReplaceAllString(html, "")

	// Decode common HTML entities
	html = strings.ReplaceAll(html, "&amp;", "&")
	html = strings.ReplaceAll(html, "&lt;", "<")
	html = strings.ReplaceAll(html, "&gt;", ">")
	html = strings.ReplaceAll(html, "&quot;", "\"")
	html = strings.ReplaceAll(html, "&#39;", "'")
	html = strings.ReplaceAll(html, "&nbsp;", " ")
	html = strings.ReplaceAll(html, "&#x27;", "'")

	// Collapse whitespace: multiple spaces → single space, multiple newlines → double newline
	reSpaces := regexp.MustCompile(`[^\S\n]+`)
	html = reSpaces.ReplaceAllString(html, " ")
	reNewlines := regexp.MustCompile(`\n{3,}`)
	html = reNewlines.ReplaceAllString(html, "\n\n")

	// Trim each line
	lines := strings.Split(html, "\n")
	var cleaned []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}

	return strings.Join(cleaned, "\n")
}
