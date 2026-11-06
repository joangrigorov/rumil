package server

import (
	"net/http"

	"github.com/joangrigorov/rumil/internal/store"
)

// homeSessionLimit is how many sessions each Home section shows. The full list
// is one click away on the Sessions page.
const homeSessionLimit = 5

// homePage is the landing page's view model. Stats is nil when the rule counts
// could not be read, which hides the summary rather than the page.
type homePage struct {
	Title   string
	Section string
	Refresh string
	Fixed   bool
	User    string
	Running []store.ReviewSession
	Recent  []store.ReviewSession
	Stats   *store.MemoryStats
}

// home is the landing page: what is running, what finished last, and whether
// any rule is waiting for a decision.
func (s *SessionsUI) home(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	running, err := s.db.ListReviewSessions(ctx, store.ReviewSessionFilter{
		Status: store.ReviewRunning,
		Limit:  homeSessionLimit,
	})
	if err != nil {
		s.logger.Error("listing running review sessions failed", "error", err)
		s.auth.refuse(w, r, http.StatusInternalServerError, "the home page could not be read")
		return
	}

	recent, err := s.db.RecentCompletedReviewSessions(ctx, homeSessionLimit)
	if err != nil {
		s.logger.Error("listing completed review sessions failed", "error", err)
		s.auth.refuse(w, r, http.StatusInternalServerError, "the home page could not be read")
		return
	}

	// The rule summary is informational, so a failure to read it leaves the
	// session sections standing.
	var stats *store.MemoryStats
	if s.ruleStats != nil {
		if counts, statsErr := s.ruleStats(ctx); statsErr != nil {
			s.logger.Warn("counting recorded rules failed", "error", statsErr)
		} else {
			stats = &counts
		}
	}

	page := homePage{
		Title:   "Home",
		Section: "home",
		User:    adminIdentity(r),
		Running: running,
		Recent:  recent,
		Stats:   stats,
	}
	if len(running) > 0 {
		// A running review changes the page on its own, so a parked tab keeps
		// up without a line of JavaScript.
		page.Refresh = "15"
	}
	s.writePage(w, r, homeTemplates, "home.html", page)
}

var homeTemplates = parseTemplates("home.html", pageFuncs())
