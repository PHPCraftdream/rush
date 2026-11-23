package skills

import (
	"log/slog"
	"slices"
	"strings"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/fs/home"
)

// DiscoverForConfig runs skill discovery for the coordinator at session
// start. Fork note: upstream threads a pre-built skills.Manager through
// from app.New; we rejected that abstraction (see CHANGELOG.fork.md
// Section 2) and keep the simple inline discovery here.
func DiscoverForConfig(cfg *config.ConfigStore) (allSkills, activeSkills []*Skill) {
	builtin, builtinStates := DiscoverBuiltinWithStates()
	discovered := append([]*Skill(nil), builtin...)

	var userStates []*SkillState
	var userPaths []string

	opts := cfg.Config().Options
	if opts != nil && len(opts.SkillsPaths) > 0 {
		userPaths = make([]string, 0, len(opts.SkillsPaths))
		for _, pth := range opts.SkillsPaths {
			expanded := home.Long(pth)
			if strings.HasPrefix(expanded, "$") {
				if resolved, err := cfg.Resolver().ResolveValue(expanded); err == nil {
					expanded = resolved
				}
			}
			userPaths = append(userPaths, expanded)
		}
		var userSkills []*Skill
		userSkills, userStates = DiscoverWithStates(userPaths)
		discovered = append(discovered, userSkills...)
	}

	allSkills = Deduplicate(discovered)
	var disabledSkills []string
	if opts != nil {
		disabledSkills = opts.DisabledSkills
	}
	activeSkills = Filter(allSkills, disabledSkills)

	allStates := append([]*SkillState(nil), builtinStates...)
	allStates = append(allStates, userStates...)

	allStates = DeduplicateStates(allStates)

	slices.SortStableFunc(allStates, func(a, b *SkillState) int {
		return strings.Compare(strings.ToLower(a.Path), strings.ToLower(b.Path))
	})
	SetLatestStates(allStates)
	PublishStates(allStates)

	logDiscoveryStats(builtin, builtinStates, userStates, userPaths, allSkills, activeSkills, disabledSkills)
	return allSkills, activeSkills
}

// LogTurnUsage emits a per-turn diagnostic line showing which skills
// (if any) were loaded during this turn and which looked relevant based on
// a cheap keyword match against the user prompt. The goal is to surface
// "should-have-loaded but didn't" situations for later analysis.
//
// Logged at Info level under component=skills; heavy fields are elided when
// there is nothing interesting to report.
func LogTurnUsage(
	sessionID string,
	prompt string,
	activeSkills []*Skill,
	tracker *Tracker,
	before []string,
) {
	if tracker == nil || len(activeSkills) == 0 {
		return
	}

	after := tracker.LoadedNames()

	beforeSet := make(map[string]bool, len(before))
	for _, n := range before {
		beforeSet[n] = true
	}
	var loadedThisTurn []string
	for _, n := range after {
		if !beforeSet[n] {
			loadedThisTurn = append(loadedThisTurn, n)
		}
	}

	slog.Info(
		"Skill turn summary",
		"component", "skills",
		"session_id", sessionID,
		"prompt_len", len(prompt),
		"active_total", len(activeSkills),
		"loaded_total", len(after),
		"loaded_this_turn", loadedThisTurn,
	)
}

// logDiscoveryStats emits a single structured log line summarising skill
// discovery for the current session. It is intentionally low-volume: one
// line per session start.
func logDiscoveryStats(
	builtin []*Skill,
	builtinStates, userStates []*SkillState,
	userPaths []string,
	allSkills, activeSkills []*Skill,
	disabled []string,
) {
	countErrors := func(states []*SkillState) int {
		n := 0
		for _, s := range states {
			if s.State == StateError {
				n++
			}
		}
		return n
	}

	userOK := 0
	for _, s := range userStates {
		if s.State == StateNormal {
			userOK++
		}
	}

	activeNames := make([]string, 0, len(activeSkills))
	for _, s := range activeSkills {
		activeNames = append(activeNames, s.Name)
	}

	xml := ToPromptXML(activeSkills)

	slog.Info(
		"Skill discovery complete",
		"component", "skills",
		"builtin_ok", len(builtin),
		"builtin_errors", countErrors(builtinStates),
		"user_ok", userOK,
		"user_errors", countErrors(userStates),
		"user_paths", len(userPaths),
		"deduped_total", len(allSkills),
		"active", len(activeSkills),
		"disabled", len(disabled),
		"prompt_bytes", len(xml),
		"prompt_tok_est", ApproxTokenCount(xml),
		"active_names", activeNames,
	)
}
