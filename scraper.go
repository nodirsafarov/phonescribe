// scraper.go — Scrape phone numbers from orginfo.uz by company name.
//
// Flow:
//   1. Search:  GET /ru/search/all/?q={normalized}
//      Collect every <a class="og-card"> on the page; mark each active or
//      liquidated based on the "Ликвидирована" badge.
//   2. Filter:  drop liquidated cards entirely.
//   3. Pick:    if 1 active → use it. If many → Picker chooses (Gemini, with
//      fallback to "first active" when no picker is configured).
//   4. Detail:  GET picked card's URL, extract <a itemprop="telephone">.
//
// CRITICAL: orginfo's search treats the "OOO \"NAME\"" legal-form prefix as
// literal text — searching for `OOO "IBRAGIM TRANS"` returns 0 hits while
// `IBRAGIM TRANS` returns 5. NormalizeCompanyName strips the prefix and
// surrounding quotes before we hit the network. Verified 2026-05-18.

package main

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gocolly/colly/v2"
)

const (
	orginfoBase       = "https://orginfo.uz"
	orginfoSearchPath = "/ru/search/all/"

	scraperUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) " +
		"Chrome/120.0.0.0 Safari/537.36"

	scraperRequestTimeout = 30 * time.Second

	liquidatedBadge = "Ликвидирована"
)

// Reason codes used by the caller (job runner) to label each UI event.
const (
	ReasonOK              = ""
	ReasonNoResults       = "NO_RESULTS"
	ReasonAllLiquidated   = "ALL_LIQUIDATED"
	ReasonNoPhone         = "NO_PHONE"
	ReasonEmptyAfterClean = "EMPTY_AFTER_CLEAN"
)

// nameCleanRE strips legal-form prefixes from the start of the company name.
// Covers Latin and Cyrillic spellings of the most common forms in Uzbek/Russian
// business records: OOO/ООО (LLC), MChJ/МЧЖ (Uzbek LLC), AJ/АЖ (joint-stock),
// OJ/ОЈ, IP/ЧП (sole proprietor), DK/ДК (state enterprise).
var nameCleanRE = regexp.MustCompile(`(?i)^\s*(ooo|ооо|mchj|мчж|aj|аж|oj|оj|ip|чп|dk|дк)\s+`)

// quoteChars covers ASCII double-quote, smart/typographic variants leaked by
// Word/Excel/Sheets autocorrect, and the angle-quote forms common in Russian.
var quoteChars = map[rune]bool{
	'"': true, '\u00ab': true, '\u00bb': true,
	'\u2018': true, '\u2019': true, '\u201c': true, '\u201d': true,
	'`': true,
}

// NormalizeCompanyName prepares a raw cell value for orginfo search.
//
//	"OOO \"IBRAGIM TRANS\""        → "IBRAGIM TRANS"
//	"ООО «Bunyod Asadbek Truck»"   → "Bunyod Asadbek Truck"
//	"  Mchj \"SHAVKAT KENJAEVICH\"" → "SHAVKAT KENJAEVICH"
func NormalizeCompanyName(raw string) string {
	s := strings.TrimSpace(raw)
	s = nameCleanRE.ReplaceAllString(s, "")

	// Iteratively strip outer quote-pairs. Handles nested cases like ""Foo""
	// without overshooting on asymmetric input.
	for {
		s = strings.TrimSpace(s)
		if utf8.RuneCountInString(s) < 2 {
			break
		}
		first, firstSize := utf8.DecodeRuneInString(s)
		last, lastSize := utf8.DecodeLastRuneInString(s)
		if !quoteChars[first] || !quoteChars[last] {
			break
		}
		s = s[firstSize : len(s)-lastSize]
	}
	return strings.TrimSpace(s)
}

// Candidate is one organization card from the search results page.
type Candidate struct {
	Name     string
	URL      string
	INN      string
	Location string
	IsActive bool
}

// Picker chooses among multiple active candidates. Returns the index (0-based)
// of the best match for `query`, or a negative number to mean "no good match —
// fall back to first". Implementations may consult external APIs (e.g. Gemini)
// and should return promptly (≤ a few seconds).
type Picker interface {
	Pick(ctx context.Context, query string, candidates []Candidate) (int, error)
}

// ScrapeResult is the outcome of one ScrapePhone call. Phone is "" unless the
// search produced at least one active candidate AND the detail page had a
// telephone field. Reason is set whenever Phone is "" so the UI can show why.
type ScrapeResult struct {
	Phone       string
	Normalized  string
	Picked      *Candidate
	AllCount    int
	ActiveCount int
	Reason      string
}

// ScrapePhone runs the full search → filter → pick → fetch flow. picker may be
// nil; when nil, multiple active candidates collapse to the first one.
//
// Returns a non-nil error only on transport-level failures (timeouts, 5xx,
// HTML drift). A successful call with Result.Phone == "" means "no usable
// phone found" and is communicated via Result.Reason — not via err.
func ScrapePhone(ctx context.Context, rawCompanyName string, picker Picker) (ScrapeResult, error) {
	res := ScrapeResult{Normalized: NormalizeCompanyName(rawCompanyName)}

	if res.Normalized == "" {
		res.Reason = ReasonEmptyAfterClean
		return res, nil
	}

	candidates, err := searchOrginfo(res.Normalized)
	if err != nil {
		return res, err
	}
	res.AllCount = len(candidates)

	active := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.IsActive {
			active = append(active, c)
		}
	}
	res.ActiveCount = len(active)

	if len(active) == 0 {
		if len(candidates) == 0 {
			res.Reason = ReasonNoResults
		} else {
			res.Reason = ReasonAllLiquidated
		}
		return res, nil
	}

	idx := 0
	if len(active) > 1 && picker != nil {
		if pickedIdx, pickErr := picker.Pick(ctx, rawCompanyName, active); pickErr == nil && pickedIdx >= 0 && pickedIdx < len(active) {
			idx = pickedIdx
		}
	}
	picked := active[idx]
	res.Picked = &picked

	phone, err := fetchPhone(picked.URL)
	if err != nil {
		return res, err
	}
	if phone == "" {
		res.Reason = ReasonNoPhone
		return res, nil
	}
	res.Phone = phone
	return res, nil
}

// searchOrginfo loads the search results page and parses every org-card on it.
// Liquidation status is detected via the "Ликвидирована" badge inside the
// card's span.text-danger element; an active card has no such text.
func searchOrginfo(normalizedName string) ([]Candidate, error) {
	var out []Candidate

	c := colly.NewCollector(
		colly.UserAgent(scraperUserAgent),
		colly.AllowedDomains("orginfo.uz"),
		colly.IgnoreRobotsTxt(),
	)
	c.SetRequestTimeout(scraperRequestTimeout)

	c.OnHTML("a.og-card", func(e *colly.HTMLElement) {
		dangerText := strings.TrimSpace(e.ChildText("span.text-danger"))
		out = append(out, Candidate{
			Name:     strings.TrimSpace(e.ChildText("h6.card-title")),
			URL:      e.Request.AbsoluteURL(e.Attr("href")),
			INN:      strings.TrimSpace(e.ChildText("span.text-success")),
			Location: strings.TrimSpace(e.ChildText("p.text-body-tertiary")),
			IsActive: !strings.Contains(dangerText, liquidatedBadge),
		})
	})

	var visitErr error
	c.OnError(func(_ *colly.Response, err error) { visitErr = err })

	q := url.Values{}
	q.Set("q", normalizedName)
	searchURL := orginfoBase + orginfoSearchPath + "?" + q.Encode()

	if err := c.Visit(searchURL); err != nil {
		return nil, fmt.Errorf("search %s: %w", searchURL, err)
	}
	if visitErr != nil {
		return nil, fmt.Errorf("search %s: %w", searchURL, visitErr)
	}
	return out, nil
}

// fetchPhone visits an organization detail page and returns the text of the
// first <a itemprop="telephone"> element. Empty string when the page has no
// phone field (e.g. owner hid it via orginfo's paid feature).
func fetchPhone(detailURL string) (string, error) {
	var phone string

	c := colly.NewCollector(
		colly.UserAgent(scraperUserAgent),
		colly.AllowedDomains("orginfo.uz"),
		colly.IgnoreRobotsTxt(),
	)
	c.SetRequestTimeout(scraperRequestTimeout)

	c.OnHTML(`a[itemprop="telephone"]`, func(e *colly.HTMLElement) {
		if phone != "" {
			return
		}
		phone = strings.TrimSpace(e.Text)
	})

	var visitErr error
	c.OnError(func(_ *colly.Response, err error) { visitErr = err })

	if err := c.Visit(detailURL); err != nil {
		return "", fmt.Errorf("detail %s: %w", detailURL, err)
	}
	if visitErr != nil {
		return "", fmt.Errorf("detail %s: %w", detailURL, visitErr)
	}
	return phone, nil
}
