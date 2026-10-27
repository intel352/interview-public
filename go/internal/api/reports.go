package api

import (
	"net/http"
	"time"

	"github.com/weave-lab/interview-public/go/internal/auth"
)

func (a *API) HandleActivityReport(w http.ResponseWriter, r *http.Request) {
	since, err := reportSince(r.URL.Query().Get("since"), time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, "since must be a YYYY-MM-DD date")
		return
	}

	report, err := a.store.GenerateActivityReport(r.Context(), auth.UserID(r.Context()), since)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate report")
		return
	}

	// Preserve the historical "view" action while auditing before output. This
	// attempt is intentionally absent from the report snapshot above.
	if err := a.store.LogActivity(r.Context(), auth.UserID(r.Context()), "view", "report", "activity"); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to record report attempt")
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func reportSince(value string, now time.Time) (time.Time, error) {
	if value == "" {
		// Normalize before calendar arithmetic so local DST changes do not move
		// the cutoff relative to timestamps stored in UTC.
		return now.UTC().AddDate(0, 0, -30), nil
	}
	return time.Parse("2006-01-02", value)
}
