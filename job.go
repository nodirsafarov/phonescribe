// job.go — One execution of the scrape-and-write workflow.
//
// A Job reads the requested row range from the Sheet, scrapes orginfo for each
// row that needs a phone, optionally batch-writes the results back, and emits
// progress Events for the SSE handler to stream to the browser.
//
// Skip rules (decided locally, no orginfo call needed):
//   1. Column E (company name) is blank          → SKIP / EMPTY_NAME
//   2. Column H (phone) has any non-empty value  → SKIP / H_NOT_EMPTY
//
// Skip rules from scraper output:
//   3. orginfo returned zero results             → SKIP / NO_RESULTS
//   4. All results are "Ликвидирована"           → SKIP / ALL_LIQUIDATED
//   5. Picked active result has no phone field   → SKIP / NO_PHONE
//
// Writes are accumulated and committed in ONE BatchUpdate call at the end —
// so the write either lands as a single atomic-ish operation or doesn't land
// at all (in which case the per-row events still tell the user what was
// intended). Dry-run mode skips the batch write entirely.

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/api/sheets/v4"
)

// Event action labels.
const (
	ActionWritten    = "WRITTEN"
	ActionWouldWrite = "WOULD_WRITE"
	ActionSkipped    = "SKIPPED"
	ActionError      = "ERROR"
)

// Reasons in addition to the scraper's (Reason* constants in scraper.go).
const (
	ReasonEmptyName = "EMPTY_NAME"
	ReasonHNotEmpty = "H_NOT_EMPTY"
)

// Event is one progress message streamed to the browser via SSE. Only the
// fields relevant to its Type are populated; the rest are omitted via JSON
// `omitempty` tags so the wire format stays compact.
type Event struct {
	Type        string `json:"type"`
	SheetRow    int    `json:"sheetRow,omitempty"`
	Company     string `json:"company,omitempty"`
	Cleaned     string `json:"cleaned,omitempty"`
	PickedName  string `json:"pickedName,omitempty"`
	PickedINN   string `json:"pickedINN,omitempty"`
	Phone       string `json:"phone,omitempty"`
	CurrentH    string `json:"currentH,omitempty"`
	Action      string `json:"action,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Error       string `json:"error,omitempty"`
	AllCount    int    `json:"allCount,omitempty"`
	ActiveCount int    `json:"activeCount,omitempty"`

	Total           int `json:"total,omitempty"`
	Written         int `json:"written,omitempty"`
	WouldWrite      int `json:"wouldWrite,omitempty"`
	SkippedHFull    int `json:"skippedHFull,omitempty"`
	SkippedEmpty    int `json:"skippedEmpty,omitempty"`
	SkippedNotFound int `json:"skippedNotFound,omitempty"`
	Errors          int `json:"errors,omitempty"`
}

// JobConfig is one job's user-supplied parameters.
type JobConfig struct {
	SpreadsheetID string
	TabName       string
	FromRow       int
	ToRow         int
	DryRun        bool
	RequestDelay  time.Duration
}

// Job is one in-flight (or completed) run.
//
// Events are buffered in `events` slice rather than written to an unbounded
// channel, so an SSE client that disconnects and reconnects can replay every
// event from the beginning. `waiters` are notified each time a new event lands
// so SSE handlers can block efficiently between flushes.
type Job struct {
	ID      string
	Config  JobConfig
	Service *sheets.Service
	Picker  Picker

	mu       sync.Mutex
	events   []Event
	waiters  map[chan struct{}]struct{}
	finished bool

	cancel context.CancelFunc
}

func newJob(id string, cfg JobConfig, srv *sheets.Service, picker Picker) (*Job, context.Context) {
	ctx, cancel := context.WithCancel(context.Background())
	return &Job{
		ID:      id,
		Config:  cfg,
		Service: srv,
		Picker:  picker,
		waiters: make(map[chan struct{}]struct{}),
		cancel:  cancel,
	}, ctx
}

// emit appends an event and wakes all subscribers.
func (j *Job) emit(e Event) {
	j.mu.Lock()
	j.events = append(j.events, e)
	for ch := range j.waiters {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	j.mu.Unlock()
}

// markFinished is called once after Run returns; future Snapshot calls will
// report finished=true so SSE handlers know to disconnect.
func (j *Job) markFinished() {
	j.mu.Lock()
	j.finished = true
	for ch := range j.waiters {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	j.mu.Unlock()
}

// Snapshot returns all events with index >= from, plus whether the job is done.
// Callers track their own `from` cursor to avoid re-sending events on each tick.
func (j *Job) Snapshot(from int) (events []Event, finished bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if from < len(j.events) {
		events = append([]Event(nil), j.events[from:]...)
	}
	return events, j.finished
}

// Subscribe registers a wake-up channel. Each emit/markFinished call sends a
// non-blocking signal to every registered channel. The returned `unsub` MUST be
// called when the subscriber disconnects to avoid leaking the channel.
func (j *Job) Subscribe() (ch chan struct{}, unsub func()) {
	j.mu.Lock()
	defer j.mu.Unlock()
	ch = make(chan struct{}, 4)
	j.waiters[ch] = struct{}{}
	unsub = func() {
		j.mu.Lock()
		delete(j.waiters, ch)
		j.mu.Unlock()
	}
	return ch, unsub
}

// Run executes the job. Always emits a final "done" event (or "error" if it
// died early). Caller is expected to launch this in a goroutine.
func (j *Job) Run(ctx context.Context) {
	defer j.markFinished()

	rows, err := ReadERange(j.Service, j.Config.SpreadsheetID, j.Config.TabName, j.Config.FromRow, j.Config.ToRow)
	if err != nil {
		j.emit(Event{Type: "error", Error: "read range: " + err.Error()})
		return
	}

	writes := make(map[int]string)
	var stats struct {
		written, wouldWrite, hFull, empty, notFound, errs int
	}

	for i, r := range rows {
		select {
		case <-ctx.Done():
			j.emit(Event{Type: "error", Error: "cancelled"})
			return
		default:
		}

		company := strings.TrimSpace(r.Company)

		if company == "" {
			stats.empty++
			j.emit(Event{
				Type:     "row",
				SheetRow: r.SheetRow,
				Action:   ActionSkipped,
				Reason:   ReasonEmptyName,
			})
			continue
		}

		if r.Phone != "" {
			stats.hFull++
			j.emit(Event{
				Type:     "row",
				SheetRow: r.SheetRow,
				Company:  company,
				CurrentH: r.Phone,
				Action:   ActionSkipped,
				Reason:   ReasonHNotEmpty,
			})
			continue
		}

		result, scrapeErr := ScrapePhone(ctx, company, j.Picker)
		if scrapeErr != nil {
			stats.errs++
			j.emit(Event{
				Type:     "row",
				SheetRow: r.SheetRow,
				Company:  company,
				Cleaned:  result.Normalized,
				Action:   ActionError,
				Error:    scrapeErr.Error(),
			})
			j.sleepBetweenRows(ctx, i, len(rows))
			continue
		}

		if result.Phone == "" {
			stats.notFound++
			ev := Event{
				Type:        "row",
				SheetRow:    r.SheetRow,
				Company:     company,
				Cleaned:     result.Normalized,
				AllCount:    result.AllCount,
				ActiveCount: result.ActiveCount,
				Action:      ActionSkipped,
				Reason:      result.Reason,
			}
			if result.Picked != nil {
				ev.PickedName = result.Picked.Name
				ev.PickedINN = result.Picked.INN
			}
			j.emit(ev)
			j.sleepBetweenRows(ctx, i, len(rows))
			continue
		}

		action := ActionWouldWrite
		if j.Config.DryRun {
			stats.wouldWrite++
		} else {
			writes[r.SheetRow] = result.Phone
			stats.written++
			action = ActionWritten
		}

		ev := Event{
			Type:        "row",
			SheetRow:    r.SheetRow,
			Company:     company,
			Cleaned:     result.Normalized,
			Phone:       result.Phone,
			AllCount:    result.AllCount,
			ActiveCount: result.ActiveCount,
			Action:      action,
		}
		if result.Picked != nil {
			ev.PickedName = result.Picked.Name
			ev.PickedINN = result.Picked.INN
		}
		j.emit(ev)

		j.sleepBetweenRows(ctx, i, len(rows))
	}

	if !j.Config.DryRun && len(writes) > 0 {
		if err := WritePhones(j.Service, j.Config.SpreadsheetID, j.Config.TabName, writes); err != nil {
			j.emit(Event{Type: "error", Error: "batch write: " + err.Error()})
			stats.errs += len(writes)
			stats.written = 0
		}
	}

	j.emit(Event{
		Type:            "done",
		Total:           len(rows),
		Written:         stats.written,
		WouldWrite:      stats.wouldWrite,
		SkippedHFull:    stats.hFull,
		SkippedEmpty:    stats.empty,
		SkippedNotFound: stats.notFound,
		Errors:          stats.errs,
	})
}

// sleepBetweenRows honors RequestDelay between orginfo-touching iterations,
// skipping the trailing sleep on the last row. Cancellation-aware.
func (j *Job) sleepBetweenRows(ctx context.Context, currentIdx, total int) {
	if currentIdx >= total-1 || j.Config.RequestDelay <= 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(j.Config.RequestDelay):
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// JobRegistry — at most one active job, keep all jobs by ID for SSE catch-up.
// ─────────────────────────────────────────────────────────────────────────────

type JobRegistry struct {
	mu      sync.Mutex
	active  *Job
	history map[string]*Job
}

func NewJobRegistry() *JobRegistry {
	return &JobRegistry{history: make(map[string]*Job)}
}

// Start launches a new job. Returns an error if another job is still running.
func (r *JobRegistry) Start(cfg JobConfig, srv *sheets.Service, picker Picker) (*Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.active != nil {
		r.active.mu.Lock()
		stillRunning := !r.active.finished
		r.active.mu.Unlock()
		if stillRunning {
			return nil, errors.New("another job is already running — wait for it to finish or refresh the page")
		}
	}

	id := fmt.Sprintf("job-%d", time.Now().UnixNano())
	job, ctx := newJob(id, cfg, srv, picker)
	r.active = job
	r.history[id] = job
	go job.Run(ctx)
	return job, nil
}

// Get returns a job by ID — including completed jobs, so a browser tab
// reopened after the job finished can still replay all events.
func (r *JobRegistry) Get(id string) *Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.history[id]
}
