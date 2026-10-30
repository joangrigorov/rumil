package server

import (
	"errors"
	"net/http"

	"github.com/joangrigorov/rumil/internal/store"
)

// queuePage is the queue view: what waits for a worker and what is running.
type queuePage struct {
	Title   string
	Section string
	Refresh string
	Fixed   bool
	Entries []store.QueueEntry
	Count   int
	Running int
	Queued  int
}

// queueConfirmPage asks before a queued job or a running review is stopped.
type queueConfirmPage struct {
	Title   string
	Section string
	Refresh string
	Fixed   bool
	Job     *store.Job
}

// queue lists the jobs that have not finished. The page refreshes itself while
// there is work, which is the same no-JavaScript pattern the session list uses.
func (s *SessionsUI) queue(w http.ResponseWriter, r *http.Request) {
	entries, err := s.db.ListQueue(r.Context(), store.QueueListMax)
	if err != nil {
		s.logger.Error("listing the queue failed", "error", err)
		s.auth.refuse(w, r, http.StatusInternalServerError, "the queue could not be read")
		return
	}

	page := queuePage{
		Title:   "Queue",
		Section: "queue",
		Entries: entries,
		Count:   len(entries),
	}
	for _, entry := range entries {
		if entry.Status == store.StatusRunning {
			page.Running++
		} else {
			page.Queued++
		}
	}
	if len(entries) > 0 {
		page.Refresh = "5"
	}
	s.writePage(w, r, queueTemplates, "queue.html", page)
}

// cancelConfirm shows the job and what cancelling it does. It is a read, so the
// cookie may authorize it; the POST that follows needs the header.
func (s *SessionsUI) cancelConfirm(w http.ResponseWriter, r *http.Request) {
	job, err := s.db.JobByID(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrJobNotFound):
		s.auth.refuse(w, r, http.StatusNotFound, "no such job")
		return
	case err != nil:
		s.logger.Error("reading a job failed", "error", err)
		s.auth.refuse(w, r, http.StatusInternalServerError, "the job could not be read")
		return
	}
	if job.Status != store.StatusQueued && job.Status != store.StatusRunning {
		s.auth.refuse(w, r, http.StatusConflict, "this job already finished")
		return
	}

	s.writePage(w, r, queueConfirmTemplates, "queue_confirm.html", queueConfirmPage{
		Title:   "Cancel job",
		Section: "queue",
		Job:     job,
	})
}

// cancelJob stops a job. A queued job leaves the queue at once; a running one
// gets the flag its worker polls and stops before its next model request.
func (s *SessionsUI) cancelJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.db.RequestJobCancel(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrJobNotFound):
		s.auth.refuse(w, r, http.StatusNotFound, "no such job")
		return
	case errors.Is(err, store.ErrJobFinished):
		s.auth.refuse(w, r, http.StatusConflict, "this job already finished")
		return
	case err != nil:
		s.logger.Error("cancelling a job failed", "error", err)
		s.auth.refuse(w, r, http.StatusInternalServerError, "the job could not be cancelled")
		return
	}

	s.logger.Info("cancelled a job",
		"job_id", job.ID,
		"status", job.Status,
		"cancel_requested", job.CancelRequested,
	)

	if wantsHTML(r) {
		http.Redirect(w, r, "/ui/queue", http.StatusSeeOther)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"job_id":           job.ID,
		"status":           job.Status,
		"cancel_requested": job.CancelRequested,
	})
}

// queueTemplates and queueConfirmTemplates are separate sets because every
// page defines "content".
var (
	queueTemplates        = parseTemplates("queue.html", pageFuncs())
	queueConfirmTemplates = parseTemplates("queue_confirm.html", pageFuncs())
)
