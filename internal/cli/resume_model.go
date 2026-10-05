package cli

import (
	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/session"
)

// restoreSessionRuntime makes a resumed session run on the model it was SAVED
// with, rather than on whatever the profile currently defaults to.
//
// The model was already persisted and then ignored: the launch read the session,
// built the LLM client from cfg.Model, and never looked at sess.Model. So a
// mid-session /model switch was recorded faithfully and discarded on resume.
//
// It re-runs config.Resolve rather than assigning cfg.Model, BECAUSE THE MODEL IS
// NOT A LEAF: ToolFormat, temperature, the context window and every per-model
// profile entry key off the id, so a bare assignment would send the restored model
// out carrying the previous model's tool dialect and window — a worse failure than
// the one being fixed. Resolve re-applies the whole precedence chain, so --ctx,
// KLOO_MODEL and per-model profile entries behave exactly as on a fresh launch of
// that model.
//
// PROVIDER AND MODEL ARE RESTORED AS A SET, and that is the point. The saved id is
// the RESOLVED one and alias lookup is provider-scoped, so restoring the model
// alone against the current default provider is actively worse than the bug:
//
//	saved on openrouter:  model=deepseek/deepseek-v4-flash  endpoint=openrouter.ai/api/v1
//	model-only restore:   model=deepseek/deepseek-v4-flash  endpoint=lokalai.silverjrom.app/v1
//
// Today that gives a working run on the wrong model; model-only would give a hard
// model-not-found. config.Resolve does not error on an id the endpoint never
// served (it warns and continues unless --strict-model), so nothing downstream
// would have caught it.
//
// Returns a message to show the user, or "" when nothing was restored. Every path
// that declines to restore says so: a silently ignored saved model is the original
// bug wearing a new hat.
func restoreSessionRuntime(cfg *config.Config, baseFlags config.Flags, getenv func(string) string,
	profilePath string, sess *session.Session, resuming bool) string {
	if !resuming || sess == nil || sess.Model == "" {
		return ""
	}
	// An explicit flag outranks the transcript. config.Flags.Model/Provider are
	// pointers set only under fs.Changed, so nil means the user did not type it —
	// a profile default is already folded into cfg and must NOT win here.
	if baseFlags.Model != nil || baseFlags.Provider != nil {
		return ""
	}
	// A session written before provider/endpoint were persisted cannot be restored
	// safely: the id alone does not say which endpoint served it. Declining is the
	// pre-existing behaviour, so an old session resumes exactly as it does today.
	if sess.Provider == "" && sess.Endpoint == "" {
		if sess.Model != cfg.Model {
			return "this session was saved before kloo recorded which endpoint served its model, " +
				"so it is running on " + cfg.Model + " (it was saved on " + sess.Model + "). " +
				"Pass --model to pick explicitly."
		}
		return ""
	}
	if sess.Model == cfg.Model && sess.Endpoint == cfg.Endpoint {
		return "" // already right; re-resolving would be a no-op
	}

	flags := baseFlags
	flags.Model = &sess.Model
	if sess.Provider != "" {
		flags.Provider = &sess.Provider
	}
	resumed, err := config.Resolve(flags, getenv, profilePath)
	if err != nil {
		return "this session ran on " + sess.Model + " but that no longer resolves (" + err.Error() +
			"), so it is running on " + cfg.Model + "."
	}
	// Cross-check the endpoint. The profile can have been edited since, in which
	// case the provider name still resolves but to somewhere else — and sending the
	// conversation's model to a different host is the thing this function exists to
	// prevent, so it is reported rather than assumed benign.
	if sess.Endpoint != "" && resumed.Endpoint != sess.Endpoint {
		return "this session ran on " + sess.Model + " at " + sess.Endpoint +
			", but " + providerOrNone(sess.Provider) + " now resolves to " + resumed.Endpoint +
			" — running on " + resumed.Model + " there. Pass --model/--provider to override."
	}
	*cfg = resumed
	return "resumed on this session's model " + sess.Model + " (" + providerOrNone(sess.Provider) + ")"
}
