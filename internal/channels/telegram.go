package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/local/picobot/internal/chat"
)

// StartTelegram is a convenience wrapper that uses the real polling implementation
// with the standard Telegram base URL.
// allowFrom is a list of Telegram user IDs permitted to interact with the bot.
// If empty, ALL users are allowed (open mode).
func StartTelegram(ctx context.Context, hub *chat.Hub, token string, allowFrom []string) error {
	if token == "" {
		return fmt.Errorf("telegram token not provided")
	}
	base := "https://api.telegram.org/bot" + token
	return StartTelegramWithBase(ctx, hub, token, base, allowFrom)
}

// StartTelegramWithBase starts long-polling against the given base URL (e.g., https://api.telegram.org/bot<TOKEN> or a test server URL).
// allowFrom restricts which Telegram user IDs may send messages. Empty means allow all.
func StartTelegramWithBase(ctx context.Context, hub *chat.Hub, token, base string, allowFrom []string) error {
	if base == "" {
		return fmt.Errorf("base URL is required")
	}

	// Build a fast lookup set for allowed user IDs.
	allowed := make(map[string]struct{}, len(allowFrom))
	for _, id := range allowFrom {
		allowed[id] = struct{}{}
	}

	client := &http.Client{Timeout: 45 * time.Second}

	// inbound polling goroutine
	go func() {
		offset := int64(0)
		for {
			select {
			case <-ctx.Done():
				log.Println("telegram: stopping inbound polling")
				return
			default:
			}

			values := url.Values{}
			values.Set("offset", strconv.FormatInt(offset, 10))
			values.Set("timeout", "30")
			u := base + "/getUpdates"
			resp, err := client.PostForm(u, values)
			if err != nil {
				log.Printf("telegram getUpdates error: %v", err)
				time.Sleep(1 * time.Second)
				continue
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var gu struct {
				Ok     bool `json:"ok"`
				Result []struct {
					UpdateID int64 `json:"update_id"`
					Message  *struct {
						MessageID int64 `json:"message_id"`
						From      *struct {
							ID int64 `json:"id"`
						} `json:"from"`
						Chat struct {
							ID int64 `json:"id"`
						} `json:"chat"`
						Text string `json:"text"`
					} `json:"message"`
				} `json:"result"`
			}
			if err := json.Unmarshal(body, &gu); err != nil {
				log.Printf("telegram: invalid getUpdates response: %v", err)
				continue
			}
			for _, upd := range gu.Result {
				if upd.UpdateID >= offset {
					offset = upd.UpdateID + 1
				}
				if upd.Message == nil {
					continue
				}
				m := upd.Message
				fromID := ""
				if m.From != nil {
					fromID = strconv.FormatInt(m.From.ID, 10)
				}
				// Enforce allowFrom: if the list is non-empty, reject unknown senders.
				if len(allowed) > 0 {
					if _, ok := allowed[fromID]; !ok {
						log.Printf("telegram: dropping message from unauthorized user %s", fromID)
						continue
					}
				}
				chatID := strconv.FormatInt(m.Chat.ID, 10)
				hub.In <- chat.Inbound{
					Channel:   "telegram",
					SenderID:  fromID,
					ChatID:    chatID,
					Content:   m.Text,
					Timestamp: time.Now(),
				}
			}
		}
	}()

	// Subscribe to the outbound queue before launching the goroutine so the
	// registration is visible to the hub router from the moment this function returns.
	outCh := hub.Subscribe("telegram")

	// outbound sender goroutine
	go func() {
		client := &http.Client{Timeout: 10 * time.Second}
		for {
			select {
			case <-ctx.Done():
				log.Println("telegram: stopping outbound sender")
				return
			case out := <-outCh:
				sendTelegramMessage(client, base, out.ChatID, out.Content)
			}
		}
	}()

	return nil
}

// sendTelegramMessage sends a message with HTML formatting, falling back to
// plain text if Telegram rejects the HTML.
func sendTelegramMessage(client *http.Client, base, chatID, text string) {
	html := markdownToHTML(text)

	// Try HTML first
	v := url.Values{}
	v.Set("chat_id", chatID)
	v.Set("text", html)
	v.Set("parse_mode", "HTML")
	resp, err := client.PostForm(base+"/sendMessage", v)
	if err != nil {
		log.Printf("telegram sendMessage error: %v", err)
		return
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Check if Telegram accepted the HTML
	var result struct {
		Ok bool `json:"ok"`
	}
	if json.Unmarshal(body, &result) == nil && result.Ok {
		return
	}

	// Fallback: send as plain text
	v = url.Values{}
	v.Set("chat_id", chatID)
	v.Set("text", text)
	resp, err = client.PostForm(base+"/sendMessage", v)
	if err != nil {
		log.Printf("telegram sendMessage (fallback) error: %v", err)
		return
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
}

// markdownToHTML converts common markdown patterns to Telegram-compatible HTML.
// Telegram HTML supports: <b>, <i>, <u>, <s>, <code>, <pre>, <a href="">.
func markdownToHTML(md string) string {
	// Step 1: Extract code blocks and inline code to protect them from other transforms.
	// We replace them with placeholders and restore after all other conversions.
	type placeholder struct {
		tag     string
		content string
	}
	var placeholders []placeholder
	ph := func(tag, content string) string {
		idx := len(placeholders)
		placeholders = append(placeholders, placeholder{tag, content})
		return fmt.Sprintf("\x00PH%d\x00", idx)
	}

	s := md

	// Code blocks: ```lang\n...\n```
	reCodeBlock := regexp.MustCompile("(?s)```[a-zA-Z]*\n?(.*?)```")
	s = reCodeBlock.ReplaceAllStringFunc(s, func(m string) string {
		inner := reCodeBlock.FindStringSubmatch(m)[1]
		inner = strings.TrimSpace(inner)
		// Escape HTML inside code
		inner = escapeHTML(inner)
		return ph("pre", inner)
	})

	// Inline code: `...`
	reInlineCode := regexp.MustCompile("`([^`]+)`")
	s = reInlineCode.ReplaceAllStringFunc(s, func(m string) string {
		inner := reInlineCode.FindStringSubmatch(m)[1]
		inner = escapeHTML(inner)
		return ph("code", inner)
	})

	// Step 2: Escape HTML in the remaining text (outside code blocks)
	s = escapeHTML(s)

	// Step 3: Convert markdown tables to plain text alignment.
	// Telegram doesn't support tables, so convert to a readable format.
	s = convertTables(s)

	// Step 4: Apply formatting conversions.

	// Headers: ## Text → <b>Text</b>
	reHeader := regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	s = reHeader.ReplaceAllString(s, "<b>$1</b>")

	// Bold: **text** or __text__
	reBold := regexp.MustCompile(`\*\*(.+?)\*\*|__(.+?)__`)
	s = reBold.ReplaceAllStringFunc(s, func(m string) string {
		sub := reBold.FindStringSubmatch(m)
		inner := sub[1]
		if inner == "" {
			inner = sub[2]
		}
		return "<b>" + inner + "</b>"
	})

	// Italic: *text* or _text_ (but not inside words for underscores)
	reItalic := regexp.MustCompile(`(?:^|[\s(])\*([^\s*].*?[^\s*])\*(?:[\s).,!?]|$)|(?:^|[\s(])_([^\s_].*?[^\s_])_(?:[\s).,!?]|$)`)
	s = reItalic.ReplaceAllStringFunc(s, func(m string) string {
		sub := reItalic.FindStringSubmatch(m)
		inner := sub[1]
		if inner == "" {
			inner = sub[2]
		}
		// Preserve leading/trailing whitespace from the match
		prefix := ""
		if len(m) > 0 && (m[0] == ' ' || m[0] == '\t' || m[0] == '(') {
			prefix = string(m[0])
		}
		suffix := ""
		if len(m) > 0 {
			last := m[len(m)-1]
			if last == ' ' || last == ')' || last == '.' || last == ',' || last == '!' || last == '?' {
				suffix = string(last)
			}
		}
		return prefix + "<i>" + inner + "</i>" + suffix
	})

	// Strikethrough: ~~text~~
	reStrike := regexp.MustCompile(`~~(.+?)~~`)
	s = reStrike.ReplaceAllString(s, "<s>$1</s>")

	// Links: [text](url)
	reLink := regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	s = reLink.ReplaceAllString(s, `<a href="$2">$1</a>`)

	// Blockquotes: > text → text (just remove the marker, Telegram doesn't support blockquotes in HTML)
	reBlockquote := regexp.MustCompile(`(?m)^&gt;\s?(.*)$`)
	s = reBlockquote.ReplaceAllString(s, "$1")

	// Horizontal rules: --- or *** → ———
	reHR := regexp.MustCompile(`(?m)^[-*]{3,}$`)
	s = reHR.ReplaceAllString(s, "———")

	// Step 5: Restore placeholders
	for i, p := range placeholders {
		token := fmt.Sprintf("\x00PH%d\x00", i)
		s = strings.ReplaceAll(s, token, "<"+p.tag+">"+p.content+"</"+p.tag+">")
	}

	return strings.TrimSpace(s)
}

// convertTables converts markdown tables to a simple aligned text format.
func convertTables(s string) string {
	lines := strings.Split(s, "\n")
	var result []string
	i := 0
	for i < len(lines) {
		line := lines[i]
		// Detect table: line contains | and next line is separator (|---|)
		if strings.Contains(line, "|") && i+1 < len(lines) && isTableSeparator(lines[i+1]) {
			// Parse the table
			var rows [][]string
			for i < len(lines) && strings.Contains(lines[i], "|") {
				if !isTableSeparator(lines[i]) {
					cells := parseTableRow(lines[i])
					if len(cells) > 0 {
						rows = append(rows, cells)
					}
				}
				i++
			}
			// Format as key-value pairs or simple text
			if len(rows) > 0 {
				result = append(result, formatTable(rows)...)
			}
			continue
		}
		result = append(result, line)
		i++
	}
	return strings.Join(result, "\n")
}

func isTableSeparator(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.Contains(trimmed, "|") {
		return false
	}
	// Check if it's mostly dashes and pipes
	cleaned := strings.ReplaceAll(trimmed, "|", "")
	cleaned = strings.ReplaceAll(cleaned, "-", "")
	cleaned = strings.ReplaceAll(cleaned, ":", "")
	cleaned = strings.TrimSpace(cleaned)
	return cleaned == ""
}

func parseTableRow(line string) []string {
	trimmed := strings.TrimSpace(line)
	trimmed = strings.Trim(trimmed, "|")
	parts := strings.Split(trimmed, "|")
	var cells []string
	for _, p := range parts {
		cell := strings.TrimSpace(p)
		if cell != "" {
			cells = append(cells, cell)
		}
	}
	return cells
}

func formatTable(rows [][]string) []string {
	if len(rows) == 0 {
		return nil
	}
	var out []string
	// If first row looks like a header and has >2 columns, format with header
	if len(rows) > 1 && len(rows[0]) >= 2 {
		header := rows[0]
		// Check if it's a 2-column key-value table (common for data display)
		if len(header) == 2 {
			for _, row := range rows[1:] {
				if len(row) >= 2 {
					out = append(out, row[0]+": "+row[1])
				} else if len(row) == 1 {
					out = append(out, row[0])
				}
			}
			if len(rows) == 1 {
				out = append(out, header[0]+": "+header[1])
			}
			return out
		}
		// Multi-column: format as "header1: val1 | header2: val2 | ..."
		for _, row := range rows[1:] {
			var parts []string
			for j, cell := range row {
				if j < len(header) {
					parts = append(parts, header[j]+": "+cell)
				} else {
					parts = append(parts, cell)
				}
			}
			out = append(out, strings.Join(parts, "  |  "))
		}
		return out
	}
	// Single row or simple: just join cells
	for _, row := range rows {
		out = append(out, strings.Join(row, "  |  "))
	}
	return out
}

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
