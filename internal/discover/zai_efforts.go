package discover

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
)

const ZAIEffortDocsURL = "https://docs.z.ai/guides/capabilities/thinking.md"

var (
	zaiDocumentModel = regexp.MustCompile(`(?i)\bGLM-[0-9]+(?:\.[0-9]+)?(?:-[A-Z0-9]+)*\b`)
	zaiDocumentTier  = regexp.MustCompile("`(none|minimal|low|medium|high|xhigh|max|ultra)`")
)

var effortOrder = map[string]int{
	"none": 0, "minimal": 1, "low": 2, "medium": 3,
	"high": 4, "xhigh": 5, "max": 6, "ultra": 7,
}

// DiscoverZAIEffortDocs reads explicit per-model levels from Z.AI's own
// published Markdown. It fails closed when the page no longer states them.
func DiscoverZAIEffortDocs(ctx context.Context) ([]catwalk.Model, error) {
	return discoverZAIEffortDocs(ctx, httpClient, ZAIEffortDocsURL)
}

func discoverZAIEffortDocs(ctx context.Context, client *http.Client, endpoint string) ([]catwalk.Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch Z.AI reasoning documentation: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch Z.AI reasoning documentation: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if len(data) == 1<<20 {
		return nil, fmt.Errorf("Z.AI reasoning documentation exceeds size limit")
	}
	models := parseZAIEffortDocs(string(data))
	if len(models) == 0 {
		return nil, fmt.Errorf("Z.AI reasoning documentation has no explicit per-model effort levels")
	}
	return models, nil
}

func parseZAIEffortDocs(document string) []catwalk.Model {
	var models []catwalk.Model
	seen := make(map[string]struct{})
	for line := range strings.SplitSeq(document, "\n") {
		if strings.Contains(line, "In the Coding Plan request:") {
			break
		}
		if !strings.Contains(strings.ToLower(line), "supported") {
			continue
		}
		names := zaiDocumentModel.FindAllString(line, -1)
		tiers := zaiDocumentTier.FindAllStringSubmatch(line, -1)
		if len(names) == 0 || len(tiers) == 0 {
			continue
		}
		levels := make([]string, 0, len(tiers))
		for _, tier := range tiers {
			if !slices.Contains(levels, tier[1]) {
				levels = append(levels, tier[1])
			}
		}
		slices.SortFunc(levels, func(a, b string) int { return effortOrder[a] - effortOrder[b] })
		for _, name := range names {
			id := strings.ToLower(name)
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			models = append(models, catwalk.Model{ID: id, ReasoningLevels: levels, CanReason: true})
		}
	}
	return models
}
