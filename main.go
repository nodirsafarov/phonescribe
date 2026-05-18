// main.go — entry: parse flags, build sheets client and (optional) Gemini
// picker, start the HTTP server.
//
// Required: --sheet-id flag OR SHEET_ID env var (the spreadsheet to operate on).
// Optional: GEMINI_API_KEY env var (enables the Gemini picker for choosing
// among multiple active candidates; absent means "use first active result").

package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP bind address")
	sheetID := flag.String("sheet-id", "", "Google Sheets spreadsheet ID (required; or set SHEET_ID env var)")
	delaySec := flag.Int("delay", 3, "seconds to sleep between orginfo requests")
	flag.Parse()

	if strings.TrimSpace(*sheetID) == "" {
		*sheetID = strings.TrimSpace(os.Getenv("SHEET_ID"))
	}
	if *sheetID == "" {
		log.Fatal("--sheet-id flag or SHEET_ID env var is required " +
			"(find it in your Sheets URL: docs.google.com/spreadsheets/d/<THIS>/edit)")
	}

	ctx := context.Background()
	sheetsSrv, err := NewSheetsService(ctx)
	if err != nil {
		log.Fatalf("sheets auth: %v", err)
	}

	// Construct the Picker interface only when we have a real picker, to
	// avoid Go's typed-nil-interface gotcha (a non-nil interface holding a
	// nil *GeminiPicker would make picker != nil checks lie downstream).
	var picker Picker
	if gp := NewGeminiPicker(os.Getenv("GEMINI_API_KEY")); gp != nil {
		picker = gp
		log.Printf("Gemini picker: ENABLED (model=%s)", gp.Model)
	} else {
		log.Printf("Gemini picker: disabled — set GEMINI_API_KEY to enable")
	}

	srv, err := NewServer(sheetsSrv, *sheetID, picker, time.Duration(*delaySec)*time.Second)
	if err != nil {
		log.Fatalf("server init: %v", err)
	}
	log.Printf("Sheet ID: %s", *sheetID)
	log.Printf("Request delay: %ds between rows", *delaySec)
	log.Printf("READY → open http://%s in your browser", *addr)

	if err := http.ListenAndServe(*addr, srv.Routes()); err != nil {
		log.Fatalf("server: %v", err)
	}
}
