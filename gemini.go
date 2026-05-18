// gemini.go — Optional candidate-picker backed by Google's Gemini REST API.
//
// Used by ScrapePhone when the orginfo search returns >1 active candidate for
// a single company name. Gemini sees the raw user query plus the candidate
// names and returns the index of the closest semantic match. If the API call
// fails or returns garbage, ScrapePhone silently falls back to the first
// active candidate — so Gemini is a quality boost, never a hard dependency.
//
// Enable by setting the GEMINI_API_KEY environment variable. Disable by
// leaving it unset (no picker is constructed).
//
// Cost: each call is ~50-200 input tokens + ~5 output tokens against Gemini
// 2.0/2.5 Flash — well below 1 cent per request at current pricing.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	geminiDefaultModel = "gemini-2.0-flash"
	geminiTimeout      = 15 * time.Second
)

// GeminiPicker implements scraper.Picker against Google's REST API.
type GeminiPicker struct {
	APIKey string
	Model  string // defaults to geminiDefaultModel when empty
	HTTP   *http.Client
}

// NewGeminiPicker constructs a picker. Returns nil when apiKey is empty so the
// caller can pass the result straight through without nil-checking.
func NewGeminiPicker(apiKey string) *GeminiPicker {
	if strings.TrimSpace(apiKey) == "" {
		return nil
	}
	return &GeminiPicker{
		APIKey: apiKey,
		Model:  geminiDefaultModel,
		HTTP:   &http.Client{Timeout: geminiTimeout},
	}
}

// Pick implements Picker. Returns -1 when Gemini's reply does not parse to a
// valid candidate index; callers must treat -1 as "fall back to default".
func (g *GeminiPicker) Pick(ctx context.Context, query string, candidates []Candidate) (int, error) {
	if g == nil || g.APIKey == "" || len(candidates) == 0 {
		return -1, nil
	}

	model := g.Model
	if model == "" {
		model = geminiDefaultModel
	}

	prompt := buildPickerPrompt(query, candidates)

	reqBody, err := json.Marshal(map[string]any{
		"contents": []any{
			map[string]any{
				"parts": []any{
					map[string]any{"text": prompt},
				},
			},
		},
		"generationConfig": map[string]any{
			"temperature":     0,
			"maxOutputTokens": 8,
			"responseMimeType": "text/plain",
		},
	})
	if err != nil {
		return -1, fmt.Errorf("gemini: marshal request: %w", err)
	}

	endpoint := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent",
		model,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return -1, fmt.Errorf("gemini: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", g.APIKey)

	resp, err := g.HTTP.Do(req)
	if err != nil {
		return -1, fmt.Errorf("gemini: http: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return -1, fmt.Errorf("gemini: status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return -1, fmt.Errorf("gemini: decode response: %w", err)
	}
	if len(parsed.Candidates) == 0 || len(parsed.Candidates[0].Content.Parts) == 0 {
		return -1, fmt.Errorf("gemini: empty response")
	}

	answer := strings.TrimSpace(parsed.Candidates[0].Content.Parts[0].Text)
	answer = strings.Trim(answer, "`. \n")

	idx, err := strconv.Atoi(answer)
	if err != nil {
		return -1, fmt.Errorf("gemini: non-integer reply %q", answer)
	}
	if idx < 0 || idx >= len(candidates) {
		return -1, nil
	}
	return idx, nil
}

// buildPickerPrompt instructs Gemini to return ONLY the integer index of the
// best match. The format constraint (single number, no other text) keeps the
// reply parseable with strconv.Atoi.
func buildPickerPrompt(query string, candidates []Candidate) string {
	var b strings.Builder
	b.WriteString("You are matching a user-typed company name to one of several official records ")
	b.WriteString("from a business registry. Pick the SINGLE best match by name similarity, ")
	b.WriteString("accounting for transliteration (Latin ↔ Cyrillic), legal-form suffixes ")
	b.WriteString("(\"mas`uliyati cheklangan jamiyati\", \"aksiyadorlik jamiyati\", LLC, OOO, MChJ), ")
	b.WriteString("and common typos.\n\n")
	b.WriteString("USER QUERY: ")
	b.WriteString(query)
	b.WriteString("\n\nCANDIDATES:\n")
	for i, c := range candidates {
		fmt.Fprintf(&b, "%d: %s", i, c.Name)
		if c.Location != "" {
			fmt.Fprintf(&b, "   [%s]", c.Location)
		}
		b.WriteByte('\n')
	}
	b.WriteString("\nReply with ONLY the integer index of the best match (0-based). ")
	b.WriteString("If none of the candidates is a plausible match, reply with -1. ")
	b.WriteString("Output exactly one integer and nothing else.")
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
