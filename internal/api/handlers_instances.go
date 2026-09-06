package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/httpx"
)

// handleListInstances returns the WhatsApp accounts the panel may choose
// between when a campaign is created.
//
// What it returns is deliberately small: an id, a key, a display name, an
// optional phone label, whether the account is switched on and whether
// credentials are present. The Green API instance id and token are not part of
// the type this is built from and cannot be reached from here, so no change to
// this handler can start leaking them. The panel needs to know which number a
// campaign will send through, not how to authenticate as it.
func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	stored, err := s.deps.Instances.List(r.Context(), nil)
	if err != nil {
		httpx.Internal(w, s.log, err, "list whatsapp accounts")
		return
	}
	if stored == nil {
		stored = []domain.WhatsAppInstance{}
	}

	httpx.JSON(w, http.StatusOK, s.deps.Registry.Describe(stored))
}

// handleInstanceStates reports live connectivity for every account.
//
// Each account is asked through its own client, and one account being
// unreachable is reported as that account's problem rather than failing the
// request: the panel should still be able to show that the first number is
// authorised while the second one is not.
func (s *Server) handleInstanceStates(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	type stateView struct {
		ID         int64  `json:"id"`
		Key        string `json:"instance_key"`
		Name       string `json:"name"`
		PhoneLabel string `json:"phone_label"`
		Enabled    bool   `json:"enabled"`
		Configured bool   `json:"configured"`
		State      string `json:"state"`
		Authorized bool   `json:"authorized"`
		Message    string `json:"message,omitempty"`
	}

	out := make([]stateView, 0)

	for _, inst := range s.deps.Registry.All() {
		view := stateView{
			ID:         inst.ID,
			Key:        inst.Key,
			Name:       inst.Name,
			PhoneLabel: inst.PhoneLabel,
			Enabled:    inst.Enabled,
			Configured: inst.Configured(),
		}

		switch {
		case !view.Configured:
			view.State = "not_configured"
			view.Message = "GREEN_API_INSTANCE_ID" + envSuffixFor(inst.ID) +
				" және GREEN_API_TOKEN" + envSuffixFor(inst.ID) + " енгізілмеген"
		case !inst.Enabled:
			view.State = "disabled"
			view.Message = "Аккаунт өшірілген: хабарлама қабылданбайды және жіберілмейді"
		default:
			state, err := inst.Client.State(ctx)
			if err != nil {
				view.State = "error"
				view.Message = err.Error()
			} else {
				view.State = state.State
				view.Authorized = state.Authorized
			}
		}

		out = append(out, view)
	}

	httpx.JSON(w, http.StatusOK, out)
}

// envSuffixFor renders the environment-variable suffix an account's
// credentials come from, so the panel can tell an operator exactly which
// variables are missing. It names variables, never their values.
func envSuffixFor(id int64) string {
	if id <= 1 {
		return ""
	}
	return "_" + strconv.FormatInt(id, 10)
}
