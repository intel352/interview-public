package api

import (
	"net/http"
	"time"

	"github.com/weave-lab/interview-public/go/internal/auth"
)

func (a *API) HandleActivityReport(w http.ResponseWriter, r *http.Request) {
	since := time.Now().AddDate(0, 0, -30) // last 30 days

	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since must be a YYYY-MM-DD date")
			return
		}
		since = t
	}

	report, err := a.store.GenerateActivityReport(r.Context(), since)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate report")
		return
	}

	if err := a.store.LogActivity(r.Context(), auth.UserID(r.Context()), "report_attempt", "report", "activity"); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to record report attempt")
		return
	}
	writeJSON(w, http.StatusOK, report)
}
