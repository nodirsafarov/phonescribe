<div align="center">

# PhoneScribe

**Browser-based Go tool that fills in missing phone numbers in a Google Sheet by scraping [orginfo.uz](https://orginfo.uz/) — safely.**

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Status](https://img.shields.io/badge/status-working-success)]()

</div>

---

Logistics dispatchers in Uzbekistan keep daily worksheets of carrier companies. For each LLC (`OOO "…"`) they have to look up a phone number on the public business registry [orginfo.uz](https://orginfo.uz/) and paste it back into the spreadsheet — by hand, hundreds of rows a week.

**PhoneScribe automates that lookup loop with three safety rails:**
1. It **never overwrites** an existing cell.
2. **Dry-run is the default** — nothing reaches the Sheet without an explicit confirmation.
3. **Liquidated companies are filtered out** before a phone is even fetched.

When the registry returns multiple plausible matches, an **optional Gemini AI picker** disambiguates by semantic name similarity (handling Latin ↔ Cyrillic transliteration, legal-form suffixes, and typos).

---

## 🧑‍💼 Who's this for?

Anywhere you have a spreadsheet column of company names and need the matching phone numbers, PhoneScribe replaces the hand-search loop.

| Role | Why it helps |
|---|---|
| **Logistics dispatchers** | The original use case — replace 30 min of daily copy-paste with one button |
| **B2B sales teams** | Cold-outreach prospect lists: company names in, phones out |
| **Recruiters & HR** | Bulk lookup of HR contacts for sourcing pipelines |
| **Market researchers** | Build segmented contact databases for any industry vertical |
| **Compliance / audit** | Verify counterparties are still active — liquidated ones flagged automatically |
| **Insurance / brokerage** | Keep client business-contact records up to date |
| **Tender consultants** | Pull winning-bidder contact info for follow-up |

### Concrete scenarios

- **Daily dispatch roster** — 50 new carrier LLCs arrive each morning. Manual lookup: ~30 minutes. PhoneScribe: ~3 minutes, dry-run included.
- **Sales outreach prep** — "200 logistics firms in Tashkent for an IT pitch." Names from official stats, phones missing. Done in 10 minutes.
- **Counterparty audit** — 80 vendors to verify. Active companies get phones written; `Ликвидирована` ones are surfaced as `no data` so the analyst can flag them.
- **Academic research** — "Agricultural LLCs in Qashqadaryo region" — 300 companies. Contact info filled overnight.

---

## ✨ Features

- 🌐 **Web UI** at `http://localhost:8080` — no CLI gymnastics, just pick a tab and a row range
- 📊 **Google Sheets API** — reads and writes directly, no manual `.xlsx` export/import
- 🧹 **Smart name normalization** — strips `OOO`/`ООО`/`MChJ`/`«»` quotes that break orginfo's search
- 🪦 **Liquidation filter** — skips `Ликвидирована` companies and surfaces them in the UI as `ma'lumot chiqmadi`
- 🧠 **Optional Gemini AI picker** — when many active candidates exist, AI picks the best name match
- 🔒 **Safe by design** — only writes to empty `H` cells; `dry-run` shows you exactly what would change
- 📡 **Live progress via Server-Sent Events** — see each row resolve in real time
- 🪶 **Single static binary** — UI embedded with `//go:embed`, no external assets

---

## 🎬 What you see

```
┌───────────────────────────────────────────────────────────────────────┐
│ PhoneScribe — orginfo phone scraper           Gemini: ON              │
├───────────────────────────────────────────────────────────────────────┤
│ Sheet tab: [ Задача 13.05.2026 ▼ ]   From: [2]   To: [51]             │
│ [ Dry-run ]   [ ⚠ WRITE ]                          [ Status: running ]│
├──────┬───────────────────────────┬──────────────┬─────────────────────┤
│ Row  │ Company                   │ Phone        │ Action              │
├──────┼───────────────────────────┼──────────────┼─────────────────────┤
│   2  │ OOO "IBRAGIM TRANS"       │ 942344343    │ WOULD_WRITE         │
│      │ → 302990990 "IBRAGIM TR…  │              │                     │
│   3  │ OOO "BUNYOD ASADBEK"      │ —            │ SKIPPED  H_NOT_EMPTY│
│      │                           │ 99 343 33 68 │                     │
│   4  │ OOO "OLD INACTIVE"        │ —            │ SKIPPED ALL_LIQUI…  │
│      │ → (2/5 active)            │              │                     │
└──────┴───────────────────────────┴──────────────┴─────────────────────┘
Wrote 0  •  Would-write 1  •  H full 1  •  Not found 1  •  Errors 0
```

---

## 🔧 How it works

```
┌───────────┐   read E:H    ┌──────────────┐                ┌───────────────────┐
│  Browser  │ ───────────▶ │  Go server   │ ─── search ──▶ │   orginfo.uz      │
│  (SSE)    │ ◀─────────── │  (this app)  │ ◀── results ── │  (HTML scraping)  │
└───────────┘  live events  └──────┬───────┘                └───────────────────┘
                                   │     filter liquidated
                                   │     pick (1 active OR Gemini)
                                   │     fetch detail → phone
                                   ▼
                         ┌──────────────────┐
                         │  Google Sheets   │
                         │  BatchUpdate H   │
                         │  (empty cells)   │
                         └──────────────────┘
```

**Skip rules — decided up-front, no orginfo call wasted:**

| Reason | Trigger |
|---|---|
| `EMPTY_NAME` | column E is blank |
| `H_NOT_EMPTY` | column H already has any value (number, note, anything) |
| `NO_RESULTS` | orginfo found no matching company |
| `ALL_LIQUIDATED` | every match is marked `Ликвидирована` — *"ma'lumot chiqmadi"* |
| `NO_PHONE` | active match exists but has no phone field |

---

## 🚀 Quick start

```bash
git clone https://github.com/nodirsafarov/phonescribe.git
cd phonescribe
go build .

# One-time: download credentials.json (see "Google Cloud setup" below)

# Optional: enable AI disambiguation
export GEMINI_API_KEY="AIzaSy..."

# Run with your spreadsheet ID
./phonescribe --sheet-id "<your-spreadsheet-id-here>"

# Then open http://127.0.0.1:8080
```

The first launch prints an OAuth URL — open it in your browser, grant access, paste the verification code back into the terminal. Done once. The resulting `token.json` is reused on every subsequent run.

---

## ⚙️ Setup

### Google Cloud — OAuth credentials (~5 min, one time)

1. **Project** — [console.cloud.google.com](https://console.cloud.google.com) → top-left project switcher → **NEW PROJECT** → name it `phonescribe`.
2. **Enable API** — `APIs & Services` → `Library` → search "Google Sheets API" → **ENABLE**.
3. **OAuth consent screen** — `APIs & Services` → `OAuth consent screen` → **External** → fill app name + your email → **SAVE AND CONTINUE** through the rest → on the **Test users** step, click **+ ADD USERS** and add your own Gmail address.
4. **Credentials** — `APIs & Services` → `Credentials` → **+ CREATE CREDENTIALS** → **OAuth client ID** → **Desktop app** → name `phonescribe-desktop` → **CREATE** → click **DOWNLOAD JSON** in the modal.
5. **Place the file** — save the downloaded JSON as `credentials.json` in the project root (next to the binary).

The first run will then prompt:

```
──────────────────────────────────────────────────────────────
 PhoneScribe needs a one-time authorization to access your sheet.
 Open the URL below in a browser, grant access, then paste the
 verification code back here and press Enter.

 URL:
    https://accounts.google.com/o/oauth2/auth?...

 Code:
```

Paste the code → `token.json` is written → subsequent runs are silent.

### Gemini API key (optional, ~1 min)

For smarter candidate picking when orginfo returns multiple plausible matches:

1. [aistudio.google.com/apikey](https://aistudio.google.com/apikey) → sign in → **Create API key**
2. Export it before running:
   ```bash
   export GEMINI_API_KEY="AIzaSy..."
   ./phonescribe --sheet-id "..."
   ```

Without a key the picker falls back to "first active candidate" — still correct in most cases, just less robust on edge transliterations.

---

## 🎛️ Usage

### CLI flags

| Flag | Default | Purpose |
|---|---|---|
| `--addr` | `127.0.0.1:8080` | HTTP bind address (localhost only — never bind to `0.0.0.0` with this code) |
| `--sheet-id` | — *(required)* | Google Sheets spreadsheet ID. Find it in your Sheets URL: `docs.google.com/spreadsheets/d/`**`<THIS>`**`/edit` |
| `--delay` | `3` | Seconds to sleep between orginfo requests (politeness — orginfo blocks faster scrapers) |

You can also set `SHEET_ID` and `GEMINI_API_KEY` as environment variables.

### Browser workflow

1. **Pick a tab** from the dropdown (auto-loaded from your spreadsheet).
2. **Set the row range** — e.g. `From=2` `To=51` to process 50 data rows below the header.
3. **Dry-run first** — see every row resolve without touching the sheet.
4. Review the table.
5. If happy, hit **⚠ WRITE** and confirm the dialog.

Each row in the live table shows:
- **Row number** (matches the sheet's 1-based row)
- **Company name** (the raw E-column value, plus the orginfo card we picked underneath with its INN)
- **Phone** (the scraped number, or the *current* H value in italics for skipped rows)
- **Action**: `WRITTEN` / `WOULD_WRITE` / `SKIPPED` (with reason) / `ERROR`

---

## 🛡️ Safety guarantees

> Logistics teams keep hand-typed notes in the phone column like `Telefon o'chirilgan` ("phone is off"), `Javob bermadi` ("didn't answer"), or two phone numbers separated by `va`. Losing those would destroy real work. This tool **enforces** that they never can be.

- 🔒 **Never overwrites** — every potential write is gated by an `if existingH == ""` check that happens *after* the sheet is read, not just at scrape time.
- 🚦 **Dry-run is default** — the `⚠ WRITE` button requires an extra browser confirmation dialog *and* posts a different flag to the server.
- 🏠 **localhost only** — the server binds to `127.0.0.1` so nothing on the network can reach it.
- 🔑 **Secrets are 0600** — `credentials.json` and `token.json` are saved owner-readable only, and `.gitignore`-ed.

---

## 🏗️ Project structure

```
phonescribe/
├── main.go            entry point, CLI flags, server start
├── scraper.go         orginfo scraping + company-name normalization + liquidation filter
├── sheets.go          Google Sheets OAuth Desktop Flow + read/write helpers
├── gemini.go          optional Gemini-based picker for ambiguous matches
├── job.go             one job's state machine + SSE event stream
├── server.go          HTTP handlers (UI, /tabs, /run, /events)
├── ui.html            single-page web UI, embedded with //go:embed
├── go.mod / go.sum    Go modules
├── .gitignore         keeps credentials.json and token.json out of version control
└── README.md          this file
```

---

## 🛠️ Tech stack

| Layer | Choice | Why |
|---|---|---|
| Language | **Go 1.21+** | Single static binary, easy to deploy/share |
| HTTP | `net/http` (stdlib) | No framework needed for ~3 routes + SSE |
| UI | Vanilla HTML/CSS/JS + `//go:embed` | One binary; no Node toolchain |
| Live progress | **Server-Sent Events** | One-way streaming fits perfectly; no WebSocket overhead |
| Scraper | [`gocolly/colly/v2`](https://github.com/gocolly/colly) | Battle-tested Go scraper with goquery selectors |
| Sheets API | [`google.golang.org/api/sheets/v4`](https://pkg.go.dev/google.golang.org/api/sheets/v4) | Official Google client |
| Auth | [`golang.org/x/oauth2`](https://pkg.go.dev/golang.org/x/oauth2) Desktop Flow | User's own account, no service account needed |
| AI | Gemini REST API (no SDK) | Two-field JSON request — pulling a whole SDK would be overkill |

---

## 🌐 Adapting to other registries

PhoneScribe is wired to [orginfo.uz](https://orginfo.uz/) (Uzbekistan's open business registry), but the architecture is registry-agnostic. To target another source, only two functions in `scraper.go` need replacing:

- `searchOrginfo()` — list candidate cards from a search URL
- `fetchPhone()` — extract the phone field from a detail page

Everything else (Google Sheets I/O, skip logic, dry-run safety, SSE UI, Gemini picker) stays as-is. Reasonable retargets:

| Country | Registry |
|---|---|
| 🇷🇺 Russia | [egrul.nalog.ru](https://egrul.nalog.ru) |
| 🇰🇿 Kazakhstan | [kgd.gov.kz](https://kgd.gov.kz) |
| 🇬🇧 UK | [Companies House](https://find-and-update.company-information.service.gov.uk/) — already has a free open API, even simpler |
| 🇺🇸 USA | Per-state Secretary of State business search |
| 🇪🇺 EU | Each country's national registry (e.g. Germany's Unternehmensregister) |

The same skeleton also works for non-phone enrichment: emails, addresses, registration dates — any single field on a "search results → detail page" site.

---

## 📜 License

[MIT](LICENSE) © Nodir Safarov
