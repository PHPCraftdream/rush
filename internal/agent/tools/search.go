package tools

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/kit/sys/platform"
	"golang.org/x/net/html"
)

// GitReadToolName is the name of the read-only git tool.
const GitReadToolName = "git_read"

const (
	// gitReadTimeout bounds a single invocation. Local git read operations
	// are fast; 30 seconds is generous headroom for huge blame or log -p
	// output while bounding a pathological repo so it cannot hang the turn.
	gitReadTimeout = 30 * time.Second

	// gitLogDefaultMaxCount is the default number of commits shown by log.
	gitLogDefaultMaxCount = 20

	// gitLogHardCap is the upper bound on log's max_count; larger values
	// are clamped so the model cannot request unbounded output.
	gitLogHardCap = 200
)

// GitReadParams is the input for the git_read tool.
type GitReadParams struct {
	Operation     string `json:"operation" description:"The read-only git operation to perform. One of: status, diff, log, show, blame, branch_list"`
	Ref           string `json:"ref,omitempty" description:"A commit/ref/branch/tag (e.g. \"HEAD~1\", \"main\", \"v1.2.3\", or a range like \"HEAD~2..HEAD\"). Optional for diff/log/blame; REQUIRED for show. Must not start with '-'"`
	Path          string `json:"path,omitempty" description:"Optional pathspec limiting the operation to one file/directory, relative to the working directory. Required for blame. Must not start with '-' and must stay inside the working directory"`
	Staged        bool   `json:"staged,omitempty" description:"diff only: set true (boolean) to diff the staged changes against HEAD instead of the unstaged working-tree changes"`
	ContextLines  int    `json:"context_lines,omitempty" description:"diff only: number of context lines around hunks (passes -U<n>). 0 = git default (3)"`
	MaxCount      int    `json:"max_count,omitempty" description:"log only: maximum number of commits to show (default 20, hard cap 200 — larger values are clamped)"`
	Patch         bool   `json:"patch,omitempty" description:"log only: set true (boolean) to include each commit's diff (-p)"`
	IncludeRemote bool   `json:"include_remote,omitempty" description:"branch_list only: set true (boolean) to also list remote-tracking branches (adds -a)"`
}

// GitReadResponseMetadata describes how the git_read call was served.
type GitReadResponseMetadata struct {
	Operation string `json:"operation"`
	Truncated bool   `json:"truncated"`
}

//go:embed git_read.md.tpl
var gitReadDescriptionTmpl []byte

var gitReadDescriptionTpl = template.Must(
	template.New("gitReadDescription").
		Parse(string(gitReadDescriptionTmpl)),
)

type gitReadDescriptionData struct {
	TimeoutSeconds int
	MaxLogCommits  int
	MaxOutputLen   int
}

func gitReadDescription() string {
	return renderTemplate(gitReadDescriptionTpl, gitReadDescriptionData{
		TimeoutSeconds: int(gitReadTimeout.Seconds()),
		MaxLogCommits:  gitLogHardCap,
		MaxOutputLen:   MaxOutputLength,
	})
}

// gitReadArgv builds the git argument vector for the requested operation.
// It validates ref and path before any process is spawned, so an
// argument-injection attempt never reaches git.
func gitReadArgv(workingDir string, params GitReadParams) (args []string, clampNotice string, errResp string) {
	switch params.Operation {
	case "status":
		return []string{"status", "--porcelain=v1", "--branch"}, "", ""

	case "diff":
		args = []string{"diff"}
		if params.Staged {
			args = append(args, "--cached")
		}
		if params.ContextLines > 0 {
			args = append(args, fmt.Sprintf("-U%d", params.ContextLines))
		}
		if params.Ref != "" {
			args = append(args, params.Ref)
		}
		if params.Path != "" {
			args = append(args, "--", params.Path)
		}
		return args, "", ""

	case "log":
		n := params.MaxCount
		if n == 0 {
			n = gitLogDefaultMaxCount
		}
		if n > gitLogHardCap {
			clampNotice = fmt.Sprintf("(max_count %d clamped to %d)\n", params.MaxCount, gitLogHardCap)
			n = gitLogHardCap
		}
		args = []string{"log", "-n", fmt.Sprintf("%d", n)}
		if params.Patch {
			args = append(args, "-p")
		}
		if params.Ref != "" {
			args = append(args, params.Ref)
		}
		if params.Path != "" {
			args = append(args, "--", params.Path)
		}
		return args, clampNotice, ""

	case "show":
		if params.Ref == "" {
			return nil, "", "show requires a ref (e.g. HEAD, a branch, or a commit hash)"
		}
		args = []string{"show", params.Ref}
		if params.Path != "" {
			args = append(args, "--", params.Path)
		}
		return args, "", ""

	case "blame":
		if params.Path == "" {
			return nil, "", "blame requires a path"
		}
		args = []string{"blame"}
		if params.Ref != "" {
			args = append(args, params.Ref)
		}
		args = append(args, "--", params.Path)
		return args, "", ""

	case "branch_list":
		args = []string{"branch", "--list", "-vv"}
		if params.IncludeRemote {
			args = append(args, "-a")
		}
		return args, "", ""

	default:
		return nil, "", fmt.Sprintf(
			"unknown operation %q; supported operations: status, diff, log, show, blame, branch_list",
			params.Operation)
	}
}

// pathInsideWorkingDir rejects paths starting with '-' and paths that
// resolve outside the working directory. Shared by the git_read and
// run_command tools. Unlike view.go, there is
// no permission escape hatch: neither tool may touch anything outside
// the working dir, by construction.
func pathInsideWorkingDir(workingDir, path string) string {
	if strings.HasPrefix(path, "-") {
		return fmt.Sprintf("invalid path %q: paths must not start with '-'", path)
	}

	absWorkingDir, err := filepath.Abs(workingDir)
	if err != nil {
		return fmt.Sprintf("cannot resolve working directory %q: %v", workingDir, err)
	}

	absTarget := ""
	if filepath.IsAbs(path) {
		// Join never treats an absolute second element as a replacement
		// (filepath.Join("D:\\w", "C:\\x") is "D:\\w\\C:\\x", nonsense that
		// would silently pass the Rel check below), so absolute paths are
		// used as-is.
		absTarget, err = filepath.Abs(path)
	} else {
		absTarget, err = filepath.Abs(filepath.Join(workingDir, path))
	}
	if err != nil {
		return fmt.Sprintf("invalid path %q: %v", path, err)
	}

	rel, err := filepath.Rel(absWorkingDir, absTarget)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Sprintf(
			"path %q must stay inside the working directory", path)
	}
	return ""
}

// gitReadOutputBuffer bounds bytes retained from the child process while still
// reporting writes as fully consumed so git can finish normally.
type gitReadOutputBuffer struct {
	mu sync.Mutex
	bytes.Buffer
	limit int
}

func (b *gitReadOutputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	written := len(p)
	if remaining := b.limit - b.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return written, nil
}

func (b *gitReadOutputBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

// NewGitReadTool builds the read-only git tool scoped to workingDir.
func NewGitReadTool(workingDir string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		GitReadToolName,
		gitReadDescription(),
		func(ctx context.Context, params GitReadParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			args, clampNotice, errResp := gitReadArgv(workingDir, params)
			if errResp != "" {
				return fantasy.NewTextErrorResponse(errResp), nil
			}

			usesRef := params.Operation == "diff" || params.Operation == "log" ||
				params.Operation == "show" || params.Operation == "blame"
			if usesRef && params.Ref != "" && strings.HasPrefix(params.Ref, "-") {
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"invalid ref %q: refs must not start with '-'", params.Ref)), nil
			}

			if usesRef && params.Path != "" {
				if msg := pathInsideWorkingDir(workingDir, params.Path); msg != "" {
					return fantasy.NewTextErrorResponse(msg), nil
				}
			}

			runCtx, cancel := context.WithTimeout(ctx, gitReadTimeout)
			defer cancel()

			cmd := platform.Command(runCtx, "git", args...)
			cmd.Dir = workingDir
			out := &gitReadOutputBuffer{limit: MaxOutputLength + 1}
			cmd.Stdout = out
			cmd.Stderr = out
			err := cmd.Run()
			if err != nil {
				text := strings.TrimSpace(truncateOutput(out.String()))
				if text == "" {
					text = err.Error()
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"git %s failed: %s", params.Operation, text)), nil
			}

			raw := out.String()
			text := truncateOutput(raw)
			if clampNotice != "" {
				text = clampNotice + text
			}
			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(text),
				GitReadResponseMetadata{
					Operation: params.Operation,
					Truncated: len(raw) > MaxOutputLength,
				},
			), nil
		},
	)
}

// SearchResult represents a single search result from DuckDuckGo.
type SearchResult struct {
	Title    string
	Link     string
	Snippet  string
	Position int
}

var userAgents = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:133.0) Gecko/20100101 Firefox/133.0",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:132.0) Gecko/20100101 Firefox/132.0",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:133.0) Gecko/20100101 Firefox/133.0",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Safari/605.1.15",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Safari/605.1.15",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.0.0",
}

var acceptLanguages = []string{
	"en-US,en;q=0.9",
	"en-US,en;q=0.9,es;q=0.8",
	"en-GB,en;q=0.9,en-US;q=0.8",
	"en-US,en;q=0.5",
	"en-CA,en;q=0.9,en-US;q=0.8",
}

// maxSearchBodyBytes caps how much of the DuckDuckGo search response body we
// read into memory. A compromised or misbehaving endpoint (or a redirect to
// a large file) would otherwise let io.ReadAll buffer an unbounded amount of
// data. 8 MiB is generous for a search results page while preventing
// unbounded growth (mirrors fetch.go/sourcegraph.go's LimitReader pattern).
const maxSearchBodyBytes = 8 * 1024 * 1024

func searchDuckDuckGo(ctx context.Context, client *http.Client, query string, maxResults int) ([]SearchResult, error) {
	if maxResults <= 0 {
		maxResults = 10
	}

	searchURL := "https://lite.duckduckgo.com/lite/?q=" + url.QueryEscape(query)

	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	setRandomizedHeaders(req)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return nil, fmt.Errorf("search failed with status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSearchBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	return parseLiteSearchResults(string(body), maxResults)
}

func setRandomizedHeaders(req *http.Request) {
	req.Header.Set("User-Agent", userAgents[rand.IntN(len(userAgents))])
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", acceptLanguages[rand.IntN(len(acceptLanguages))])
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Cache-Control", "max-age=0")
	if rand.IntN(2) == 0 {
		req.Header.Set("DNT", "1")
	}
}

func parseLiteSearchResults(htmlContent string, maxResults int) ([]SearchResult, error) {
	doc, err := html.Parse(strings.NewReader(htmlContent))
	if err != nil {
		return nil, fmt.Errorf("failed to parse HTML: %w", err)
	}

	var results []SearchResult
	var currentResult *SearchResult

	var traverse func(*html.Node)
	traverse = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if n.Data == "a" && hasClass(n, "result-link") {
				if currentResult != nil && currentResult.Link != "" {
					currentResult.Position = len(results) + 1
					results = append(results, *currentResult)
					if len(results) >= maxResults {
						return
					}
				}
				currentResult = &SearchResult{Title: getTextContent(n)}
				for _, attr := range n.Attr {
					if attr.Key == "href" {
						currentResult.Link = cleanDuckDuckGoURL(attr.Val)
						break
					}
				}
			}
			if n.Data == "td" && hasClass(n, "result-snippet") && currentResult != nil {
				currentResult.Snippet = getTextContent(n)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if len(results) >= maxResults {
				return
			}
			traverse(c)
		}
	}

	traverse(doc)

	if currentResult != nil && currentResult.Link != "" && len(results) < maxResults {
		currentResult.Position = len(results) + 1
		results = append(results, *currentResult)
	}

	return results, nil
}

func hasClass(n *html.Node, class string) bool {
	for _, attr := range n.Attr {
		if attr.Key == "class" {
			if slices.Contains(strings.Fields(attr.Val), class) {
				return true
			}
		}
	}
	return false
}

func getTextContent(n *html.Node) string {
	var text strings.Builder
	var traverse func(*html.Node)
	traverse = func(node *html.Node) {
		if node.Type == html.TextNode {
			text.WriteString(node.Data)
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			traverse(c)
		}
	}
	traverse(n)
	return strings.TrimSpace(text.String())
}

func cleanDuckDuckGoURL(rawURL string) string {
	if strings.HasPrefix(rawURL, "//duckduckgo.com/l/?uddg=") {
		if _, after, ok := strings.Cut(rawURL, "uddg="); ok {
			encoded := after
			if ampIdx := strings.Index(encoded, "&"); ampIdx != -1 {
				encoded = encoded[:ampIdx]
			}
			if decoded, err := url.QueryUnescape(encoded); err == nil {
				return decoded
			}
		}
	}
	return rawURL
}

func formatSearchResults(results []SearchResult) string {
	if len(results) == 0 {
		return "No results found. Try rephrasing your search."
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d search results:\n\n", len(results))
	for _, result := range results {
		fmt.Fprintf(&sb, "%d. %s\n", result.Position, result.Title)
		fmt.Fprintf(&sb, "   URL: %s\n", result.Link)
		fmt.Fprintf(&sb, "   Summary: %s\n\n", result.Snippet)
	}
	return sb.String()
}

var (
	lastSearchMu   sync.Mutex
	lastSearchTime time.Time
)

// maybeDelaySearch adds a random delay if the last search was recent.
//
// The wait duration is computed and lastSearchTime is reserved while holding
// lastSearchMu, but the actual sleep happens after the lock is released. This
// avoids serializing concurrent callers behind a held lock (N concurrent
// sessions would otherwise queue their delays sequentially) and lets the
// sleep be interrupted via ctx cancellation instead of blocking the caller
// (and the lock, previously) for the full duration regardless of cancel/turn
// timeout.
func maybeDelaySearch(ctx context.Context) error {
	wait := reserveSearchSlot()
	if wait <= 0 {
		return nil
	}

	select {
	case <-time.After(wait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// reserveSearchSlot computes how long the caller must wait before its search
// runs and immediately reserves that slot by advancing lastSearchTime, so
// concurrent callers each get a distinct slot instead of piling up behind a
// held lock.
func reserveSearchSlot() time.Duration {
	lastSearchMu.Lock()
	defer lastSearchMu.Unlock()

	minGap := time.Duration(500+rand.IntN(1500)) * time.Millisecond
	now := time.Now()
	elapsed := now.Sub(lastSearchTime)

	var wait time.Duration
	if elapsed < minGap {
		wait = minGap - elapsed
	}
	lastSearchTime = now.Add(wait)
	return wait
}

type SourcegraphParams struct {
	Query string `json:"query" description:"The Sourcegraph search query"`
	Count int    `json:"count,omitempty" description:"Optional number of results to return (default: 10, max: 20)"`
	// ContextWindow is currently a NO-OP: rendering used to fetch each
	// matched file's FULL content over the GraphQL API just to slice out a
	// few surrounding lines, which meant every single search unconditionally
	// downloaded whole files regardless of how large they were. That fetch
	// was removed (the query now only asks for path/url/lineMatches.preview,
	// never file.content) to bound response size, at the cost of this
	// parameter no longer having any effect — only the single matched line
	// (preview) is shown. Kept in the schema rather than removed outright so
	// existing callers passing this field don't get a hard schema-validation
	// error; the description makes the current behavior explicit instead of
	// silently ignoring the field.
	ContextWindow int `json:"context_window,omitempty" description:"Deprecated / currently ignored: only the single matched line is shown, not surrounding context, to avoid downloading whole file contents on every search."`
	Timeout       int `json:"timeout,omitempty" description:"Optional timeout in seconds (max 120)"`
}

type SourcegraphResponseMetadata struct {
	NumberOfMatches int  `json:"number_of_matches"`
	Truncated       bool `json:"truncated"`
}

const SourcegraphToolName = "sourcegraph"

// maxSourcegraphBodyBytes caps how much of the Sourcegraph API HTTP response
// body we read into memory. A pathological response (huge file contents,
// verbose search) would otherwise be buffered entirely by io.ReadAll.
// 10 MiB is generous for search results while preventing unbounded growth.
const maxSourcegraphBodyBytes = 10 * 1024 * 1024

//go:embed sourcegraph.md.tpl
var sourcegraphDescriptionTmpl []byte

var sourcegraphDescriptionTpl = template.Must(
	template.New("sourcegraphDescription").
		Parse(string(sourcegraphDescriptionTmpl)),
)

type sourcegraphDescriptionData struct {
	MaxResults int
}

func sourcegraphDescription() string {
	return renderTemplate(sourcegraphDescriptionTpl, sourcegraphDescriptionData{
		MaxResults: 20,
	})
}

func NewSourcegraphTool(client *http.Client) fantasy.AgentTool {
	if client == nil {
		// SSRF-guarded for consistency with the other model-facing HTTP
		// tools — see ssrf_guard.go. The Sourcegraph endpoint itself is
		// fixed, not model-controlled, so this is defense-in-depth rather
		// than closing a direct exfiltration path.
		client = NewSSRFGuardedClient(30*time.Second, false)
	}
	return fantasy.NewParallelAgentTool(
		SourcegraphToolName,
		sourcegraphDescription(),
		func(ctx context.Context, params SourcegraphParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Query == "" {
				return fantasy.NewTextErrorResponse("Query parameter is required"), nil
			}

			if params.Count <= 0 {
				params.Count = 10
			} else if params.Count > 20 {
				params.Count = 20 // Limit to 20 results
			}

			if params.ContextWindow <= 0 {
				params.ContextWindow = 10 // Default context window
			}

			// Handle timeout with context
			requestCtx := ctx
			if params.Timeout > 0 {
				maxTimeout := 120 // 2 minutes
				if params.Timeout > maxTimeout {
					params.Timeout = maxTimeout
				}
				var cancel context.CancelFunc
				requestCtx, cancel = context.WithTimeout(ctx, time.Duration(params.Timeout)*time.Second)
				defer cancel()
			}

			type graphqlRequest struct {
				Query     string `json:"query"`
				Variables struct {
					Query string `json:"query"`
				} `json:"variables"`
			}

			request := graphqlRequest{
				Query: "query Search($query: String!) { search(query: $query, version: V2, patternType: keyword ) { results { matchCount, limitHit, resultCount, approximateResultCount, missing { name }, timedout { name }, indexUnavailable, results { __typename, ... on FileMatch { repository { name }, file { path, url }, lineMatches { preview, lineNumber, offsetAndLengths } } } } } }",
			}
			request.Variables.Query = params.Query

			graphqlQueryBytes, err := json.Marshal(request)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to marshal GraphQL request: %w", err)
			}
			graphqlQuery := string(graphqlQueryBytes)

			req, err := http.NewRequestWithContext(
				requestCtx,
				"POST",
				"https://sourcegraph.com/.api/graphql",
				bytes.NewBuffer([]byte(graphqlQuery)),
			)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to create request: %w", err)
			}

			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", "rush/1.0")

			resp, err := client.Do(req)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"Sourcegraph search failed at the network level: could not "+
						"reach https://sourcegraph.com/.api/graphql: %v. This is "+
						"not a problem with the query — it was never executed and "+
						"nothing was returned. The failure may be transient: retry "+
						"later, or fall back to local search (grep/glob).",
					err,
				)), nil
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				if len(body) > 0 {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Request failed with status code: %d, response: %s", resp.StatusCode, string(body))), nil
				}

				return fantasy.NewTextErrorResponse(fmt.Sprintf("Request failed with status code: %d", resp.StatusCode)), nil
			}
			// A truncated or unparseable response is the remote service
			// misbehaving, not the model misusing the tool and not this
			// machine being broken. By the retry-invariance criterion in
			// tools.go it is not fatal: the next call may well succeed, and
			// there is nothing about it that makes the rest of the session
			// pointless. The non-200 branch immediately above already
			// answers with a response for the same reason.
			//
			// The model cannot fix the response, so the message tells it
			// what it CAN do — retry, or fall back to local search — rather
			// than implying its query was wrong.
			body, err := io.ReadAll(io.LimitReader(resp.Body, maxSourcegraphBodyBytes))
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"Sourcegraph replied but the response could not be read: %v. The query was not at fault and may not have been executed at all. Retry it, or fall back to local search (grep/glob).",
					err)), nil
			}

			var result map[string]any
			if err = json.Unmarshal(body, &result); err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"Sourcegraph returned a response that is not valid JSON: %v. This is a problem with the service, not with the query. Retry it, or fall back to local search (grep/glob).",
					err)), nil
			}

			formattedResults, err := formatSourcegraphResults(result, params.ContextWindow, params.Count)
			if err != nil {
				return fantasy.NewTextErrorResponse("Failed to format results: " + err.Error()), nil
			}

			return fantasy.NewTextResponse(formattedResults), nil
		},
	)
}

func formatSourcegraphResults(result map[string]any, contextWindow, maxResults int) (string, error) {
	var buffer strings.Builder

	if writeSourcegraphErrors(&buffer, result) {
		return buffer.String(), nil
	}

	searchResults, err := sourcegraphSearchResults(result)
	if err != nil {
		return "", err
	}

	writeSourcegraphHeader(&buffer, searchResults)

	results, ok := searchResults["results"].([]any)
	if !ok || len(results) == 0 {
		buffer.WriteString("No results found. Try a different query.\n")
		return buffer.String(), nil
	}

	if len(results) > maxResults {
		results = results[:maxResults]
	}

	for i, res := range results {
		formatSourcegraphResult(&buffer, i, res, contextWindow)
	}

	return buffer.String(), nil
}

func writeSourcegraphErrors(buffer *strings.Builder, result map[string]any) bool {
	errors, ok := result["errors"].([]any)
	if !ok || len(errors) == 0 {
		return false
	}

	buffer.WriteString("## Sourcegraph API Error\n\n")
	for _, err := range errors {
		errMap, ok := err.(map[string]any)
		if !ok {
			continue
		}
		message, ok := errMap["message"].(string)
		if ok {
			fmt.Fprintf(buffer, "- %s\n", message)
		}
	}
	return true
}

func sourcegraphSearchResults(result map[string]any) (map[string]any, error) {
	data, ok := result["data"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid response format: missing data field")
	}

	search, ok := data["search"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid response format: missing search field")
	}

	searchResults, ok := search["results"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid response format: missing results field")
	}
	return searchResults, nil
}

func writeSourcegraphHeader(buffer *strings.Builder, searchResults map[string]any) {
	matchCount, _ := searchResults["matchCount"].(float64)
	resultCount, _ := searchResults["resultCount"].(float64)
	limitHit, _ := searchResults["limitHit"].(bool)

	buffer.WriteString("# Sourcegraph Search Results\n\n")
	fmt.Fprintf(buffer, "Found %d matches across %d results\n", int(matchCount), int(resultCount))

	if limitHit {
		buffer.WriteString("(Result limit reached, try a more specific query)\n")
	}

	buffer.WriteString("\n")
}

func formatSourcegraphResult(buffer *strings.Builder, index int, res any, contextWindow int) {
	fileMatch, ok := res.(map[string]any)
	if !ok {
		return
	}

	typeName, _ := fileMatch["__typename"].(string)
	if typeName != "FileMatch" {
		return
	}

	repo, _ := fileMatch["repository"].(map[string]any)
	file, _ := fileMatch["file"].(map[string]any)
	lineMatches, _ := fileMatch["lineMatches"].([]any)

	if repo == nil || file == nil {
		return
	}

	repoName, _ := repo["name"].(string)
	filePath, _ := file["path"].(string)
	fileURL, _ := file["url"].(string)

	fmt.Fprintf(buffer, "## Result %d: %s/%s\n\n", index+1, repoName, filePath)

	if fileURL != "" {
		fmt.Fprintf(buffer, "URL: %s\n\n", fileURL)
	}

	formatSourcegraphLineMatches(buffer, lineMatches, contextWindow)
}

func formatSourcegraphLineMatches(buffer *strings.Builder, lineMatches []any, contextWindow int) {
	for _, lm := range lineMatches {
		lineMatch, ok := lm.(map[string]any)
		if !ok {
			continue
		}
		formatSourcegraphLineMatch(buffer, lineMatch, contextWindow)
	}
}

// contextWindow is currently unused: showing surrounding lines required
// fetching each matched file's full content, which is exactly the
// unbounded-response-size problem this file no longer does. Kept as a
// parameter (rather than removed) purely so this call chain doesn't need
// reshaping if a bounded, context-aware alternative is added later — see
// the doc comment on SourcegraphParams.ContextWindow.
func formatSourcegraphLineMatch(buffer *strings.Builder, lineMatch map[string]any, contextWindow int) {
	lineNumber, _ := lineMatch["lineNumber"].(float64)
	preview, _ := lineMatch["preview"].(string)
	line := int(lineNumber)

	buffer.WriteString("```\n")
	fmt.Fprintf(buffer, "%d| %s\n", line, preview)
	buffer.WriteString("```\n\n")
}
