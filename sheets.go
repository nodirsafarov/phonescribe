// sheets.go — Google Sheets client: OAuth2 Desktop Flow + read/write helpers.
//
// First run prompts the user to visit an auth URL in their browser, grant
// access, and paste the returned code into stdin. The resulting token is
// cached in token.json next to the binary. Subsequent runs reuse the cached
// token and silently refresh it as needed.
//
// Scope: full read+write (sheets.SpreadsheetsScope). Change to
// SpreadsheetsReadonlyScope for read-only access (must delete token.json after).

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

const (
	credentialsFile = "credentials.json"
	tokenFile       = "token.json"
)

// NewSheetsService builds an authenticated *sheets.Service. On first run it
// performs the OAuth2 Desktop Flow (interactive: prints a URL, reads the code
// from stdin). On subsequent runs it reads the cached token.
func NewSheetsService(ctx context.Context) (*sheets.Service, error) {
	credBytes, err := os.ReadFile(credentialsFile)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w (download it from Google Cloud Console — see README)", credentialsFile, err)
	}
	config, err := google.ConfigFromJSON(credBytes, sheets.SpreadsheetsScope)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", credentialsFile, err)
	}
	client, err := oauthHTTPClient(ctx, config)
	if err != nil {
		return nil, err
	}
	srv, err := sheets.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return nil, fmt.Errorf("sheets.NewService: %w", err)
	}
	return srv, nil
}

func oauthHTTPClient(ctx context.Context, config *oauth2.Config) (*http.Client, error) {
	tok, err := loadToken(tokenFile)
	if err != nil {
		tok, err = exchangeAuthCode(config)
		if err != nil {
			return nil, err
		}
		if saveErr := saveToken(tokenFile, tok); saveErr != nil {
			return nil, saveErr
		}
	}
	// config.Client returns an *http.Client whose Transport auto-refreshes
	// the access_token via the refresh_token. Nothing to do manually.
	return config.Client(ctx, tok), nil
}

func exchangeAuthCode(config *oauth2.Config) (*oauth2.Token, error) {
	// AccessTypeOffline is CRITICAL — without it the response has no
	// refresh_token and the token dies after ~1 hour with no recovery.
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)

	fmt.Println()
	fmt.Println("──────────────────────────────────────────────────────────────")
	fmt.Println(" PhoneScribe needs a one-time authorization to access your sheet.")
	fmt.Println(" Open the URL below in a browser, grant access, then paste the")
	fmt.Println(" verification code back here and press Enter.")
	fmt.Println()
	fmt.Println(" URL:")
	fmt.Println("  ", authURL)
	fmt.Println()
	fmt.Print(" Code: ")

	var code string
	if _, err := fmt.Scan(&code); err != nil {
		return nil, fmt.Errorf("read auth code from stdin: %w", err)
	}

	tok, err := config.Exchange(context.Background(), code)
	if err != nil {
		return nil, fmt.Errorf("exchange auth code: %w", err)
	}
	fmt.Println(" ✓ Token saved.")
	fmt.Println("──────────────────────────────────────────────────────────────")
	fmt.Println()
	return tok, nil
}

func loadToken(path string) (*oauth2.Token, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	if err := json.NewDecoder(f).Decode(tok); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return tok, nil
}

func saveToken(path string, tok *oauth2.Token) error {
	// 0600 = owner read/write only — never world-readable.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(tok)
}

// ─────────────────────────────────────────────────────────────────────────────
// Sheet metadata + reading + writing
// ─────────────────────────────────────────────────────────────────────────────

// SheetTabs returns the visible tab titles of a spreadsheet in display order.
func SheetTabs(srv *sheets.Service, spreadsheetID string) ([]string, error) {
	resp, err := srv.Spreadsheets.
		Get(spreadsheetID).
		Fields("sheets(properties(title,hidden))").
		Do()
	if err != nil {
		return nil, fmt.Errorf("get spreadsheet metadata: %w", err)
	}
	titles := make([]string, 0, len(resp.Sheets))
	for _, sh := range resp.Sheets {
		if sh.Properties == nil || sh.Properties.Hidden {
			continue
		}
		titles = append(titles, sh.Properties.Title)
	}
	return titles, nil
}

// RowData holds the columns relevant to one job for one sheet row.
type RowData struct {
	SheetRow int    // 1-based row number — matches A1 notation directly
	Company  string // column E value, raw (post FORMATTED_VALUE rendering)
	Phone    string // column H value, raw — used to decide skip vs. write
}

// ReadERange reads columns E and H for sheet rows [fromRow, toRow] inclusive,
// both 1-based, on the named tab. The returned slice has exactly
// (toRow - fromRow + 1) entries — one per requested row — even when the
// underlying sheet has no data for trailing rows.
//
// Tab name quoting: single quotes are REQUIRED around tab names containing
// spaces, dots, or Cyrillic — e.g. 'Задача 13.05.2026'!E2:H51. The Go client
// URL-encodes the range automatically.
//
// Empty cell handling: the Sheets API truncates trailing empty cells per row
// AND drops entirely-empty trailing rows. ReadERange normalizes both away so
// the caller always sees two fields per row (even if empty strings).
func ReadERange(srv *sheets.Service, spreadsheetID, tabName string, fromRow, toRow int) ([]RowData, error) {
	if fromRow < 1 || toRow < fromRow {
		return nil, fmt.Errorf("invalid range: from=%d to=%d", fromRow, toRow)
	}

	rangeA1 := fmt.Sprintf("'%s'!E%d:H%d", escapeTabName(tabName), fromRow, toRow)

	resp, err := srv.Spreadsheets.Values.
		Get(spreadsheetID, rangeA1).
		ValueRenderOption("FORMATTED_VALUE"). // displayed string, matches what user sees
		Do()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rangeA1, err)
	}

	totalRows := toRow - fromRow + 1
	out := make([]RowData, totalRows)
	for i := 0; i < totalRows; i++ {
		out[i].SheetRow = fromRow + i
	}

	for i, row := range resp.Values {
		if i >= totalRows {
			break // defensive: should never happen, but cap at requested size
		}
		// E is index 0 in an E:H range; H is index 3 (E=0, F=1, G=2, H=3).
		if len(row) > 0 && row[0] != nil {
			out[i].Company = fmt.Sprintf("%v", row[0])
		}
		if len(row) > 3 && row[3] != nil {
			out[i].Phone = fmt.Sprintf("%v", row[3])
		}
	}
	return out, nil
}

// WritePhones writes phone strings to H-column cells in a single BatchUpdate
// API call. Map keys are 1-based sheet row numbers; values are the phone
// strings to write.
//
// ValueInputOption "RAW" stores the string verbatim — no number parsing, no
// leading-zero stripping (USER_ENTERED would convert "0501234567" to 501234567).
func WritePhones(srv *sheets.Service, spreadsheetID, tabName string, writes map[int]string) error {
	if len(writes) == 0 {
		return nil
	}
	data := make([]*sheets.ValueRange, 0, len(writes))
	for row, phone := range writes {
		cellRange := fmt.Sprintf("'%s'!H%d", escapeTabName(tabName), row)
		data = append(data, &sheets.ValueRange{
			Range:  cellRange,
			Values: [][]interface{}{{phone}},
		})
	}
	req := &sheets.BatchUpdateValuesRequest{
		ValueInputOption: "RAW",
		Data:             data,
	}
	if _, err := srv.Spreadsheets.Values.BatchUpdate(spreadsheetID, req).Do(); err != nil {
		return fmt.Errorf("batch update %d cells: %w", len(writes), err)
	}
	return nil
}

// escapeTabName escapes single quotes within a tab name by doubling them, per
// the A1-notation quoting rule. e.g. `Jon's Data` → `Jon''s Data`.
func escapeTabName(tab string) string {
	return strings.ReplaceAll(tab, "'", "''")
}
