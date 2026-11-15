package cahgen

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var versionPartRE = regexp.MustCompile(`[0-9]+`)

// modelVersion parses the numeric version out of a model id:
// claude-opus-5-5 => [5 5], gpt-6.1-sol => [6 1].
func modelVersion(model string) []int {
	id := model
	if strings.HasPrefix(id, "claude-") {
		id = strings.TrimPrefix(id, "claude-")
		if i := strings.Index(id, "-"); i >= 0 {
			id = id[i+1:]
		}
	} else {
		id = strings.TrimPrefix(id, "gpt-")
		if i := strings.LastIndex(id, "-"); i >= 0 {
			id = id[:i]
		}
	}
	var v []int
	for _, p := range versionPartRE.FindAllString(id, -1) {
		n, _ := strconv.Atoi(p)
		v = append(v, n)
	}
	return v
}

// versionCmp compares versions with missing parts treated as zero.
func versionCmp(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// codeParts splits a code into family letter and slot (0 = top).
func codeParts(code, model string) (letter string, slot int, ok bool) {
	var digits string
	if strings.HasPrefix(model, "claude-") {
		m := claudeNameRE.FindStringSubmatch(code)
		if m == nil {
			return "", 0, false
		}
		letter, digits = m[1], m[2]
	} else {
		m := codexNameRE.FindStringSubmatch(code)
		if m == nil {
			return "", 0, false
		}
		letter, digits = m[2], m[3]
	}
	if digits != "" {
		slot, _ = strconv.Atoi(digits)
	}
	return letter, slot, true
}

// CheckInvariants asserts catalog-independent rules over any Transform output.
func CheckInvariants(out Output) error {
	specs := map[string]Spec{}
	for _, s := range out.Models {
		specs[s.Slug] = s
	}
	codes := make([]string, 0, len(out.Names))
	for c := range out.Names {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	targets := map[string]string{}
	type slotModel struct {
		slot  int
		code  string
		model string
	}
	chains := map[string][]slotModel{}
	for _, c := range codes {
		n := out.Names[c]
		key := n.Slug + "@" + n.Effort
		if prev, dup := targets[key]; dup {
			return fmt.Errorf("codes %s and %s both map to %s", prev, c, key)
		}
		targets[key] = c
		s, ok := specs[n.Slug]
		if !ok {
			return fmt.Errorf("code %s: no model spec for slug %s", c, n.Slug)
		}
		if s.Model != n.Model {
			return fmt.Errorf("code %s: model %s differs from spec model %s", c, n.Model, s.Model)
		}
		found := false
		for _, l := range s.Efforts {
			found = found || l == n.Effort
		}
		if !found {
			return fmt.Errorf("code %s: effort %s not accepted by %s", c, n.Effort, n.Slug)
		}
		letter, slot, ok := codeParts(c, n.Model)
		if !ok {
			return fmt.Errorf("code %s does not fit the %s code scheme", c, n.Model)
		}
		fam := strings.SplitN(n.Model, "-", 2)[0]
		k := fam + "/" + letter + "/" + n.Effort
		chains[k] = append(chains[k], slotModel{slot, c, n.Model})
	}
	keys := make([]string, 0, len(chains))
	for k := range chains {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		chain := chains[k]
		sort.Slice(chain, func(i, j int) bool { return chain[i].slot < chain[j].slot })
		for i := 1; i < len(chain); i++ {
			hi, lo := chain[i-1], chain[i]
			if versionCmp(modelVersion(hi.model), modelVersion(lo.model)) <= 0 {
				return fmt.Errorf("slot chain %s: %s (%s) is not newer than %s (%s)", k, hi.code, hi.model, lo.code, lo.model)
			}
		}
	}
	for _, c := range out.Collisions {
		cur, ok := out.Names[c.Code]
		switch {
		case !strings.HasPrefix(c.Winner.Model, "claude-") || !strings.HasPrefix(c.Loser.Model, "gpt-"):
			return fmt.Errorf("collision %s: Claude must win over Codex", c.Code)
		case !ok || cur.Model != c.Winner.Model || cur.Effort != c.Winner.Effort:
			return fmt.Errorf("collision %s: code does not resolve to the Claude winner", c.Code)
		}
		want := fmt.Sprintf("local-cli/%s@%s", c.Loser.Slug, c.Loser.Effort)
		noted := false
		for _, n := range out.Notes {
			noted = noted || strings.HasPrefix(n, c.Code+" ") && strings.Contains(n, want)
		}
		if !noted {
			return fmt.Errorf("collision %s: missing note pointing to %s", c.Code, want)
		}
	}
	return nil
}
