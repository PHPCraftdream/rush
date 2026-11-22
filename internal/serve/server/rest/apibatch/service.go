package apibatch

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/google/uuid"
)

const (
	batchIntakeDefaultMaxRoots = 2
	// batchIntakeLeafBudget caps the parallel leaves of all live API roots of the
	// process together: the engine pool (16 slots) is shared with every tool call
	// of every model turn, so API leaves stay under half of it.
	batchIntakeLeafBudget = 8
	// batchOutputTTL is how long a terminal root keeps its live output readable.
	batchOutputTTL = 10 * time.Minute
	batchRefPrefix = "batch-cli:api:"
	batchNoKey     = "-"
)

var (
	errBatchOverloaded   = errors.New("batch intake is at capacity")
	errBatchKeyConflict  = errors.New("idempotency key was used with a different request body")
	errBatchNotFound     = errors.New("batch not found")
	errBatchUnavailable  = errors.New("batch runtime unavailable")
	errBatchNoQuestion   = errors.New("no current question to answer")
	errBatchServiceOwner = errors.New("service session is not an API service session")
)

// APIBatchService runs the batch intake of one App: one service session per
// token owns that token's roots, a watcher per live root pumps stop requests,
// settles the notice debt and releases the root.
type APIBatchService struct {
	app       *appPkg.App
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	OutputTTL time.Duration
	refs      int

	Mu       sync.Mutex
	Locks    map[string]*sync.Mutex
	Watching map[string]int // root -> max_parallel reserved while the root is live
	// answered remembers the questions already answered through the API. The
	// notice of an answered question stays unreacted until its node ends, so
	// without this a node would keep reporting awaiting_answer while it works on.
	Answered map[string]struct{}
}

func newAPIBatchService(app *appPkg.App) *APIBatchService {
	ctx, cancel := context.WithCancel(context.Background())
	return &APIBatchService{app: app, ctx: ctx, cancel: cancel, OutputTTL: batchOutputTTL, Locks: make(map[string]*sync.Mutex), Watching: make(map[string]int), Answered: make(map[string]struct{})}
}

// stop cancels every watcher and waits for them; it must run before the App shuts down.
func (s *APIBatchService) stop(ctx context.Context) error {
	s.cancel()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var APIBatchServices = struct {
	sync.Mutex
	ByApp map[*appPkg.App]*APIBatchService
}{ByApp: make(map[*appPkg.App]*APIBatchService)}

// retainAPIBatchService returns the shared service of app; every listener of the
// same App shares it so the live-root budget is process-wide.
func retainAPIBatchService(app *appPkg.App) *APIBatchService {
	APIBatchServices.Lock()
	defer APIBatchServices.Unlock()
	if existing := APIBatchServices.ByApp[app]; existing != nil {
		existing.refs++
		return existing
	}
	service := newAPIBatchService(app)
	service.refs = 1
	APIBatchServices.ByApp[app] = service
	return service
}

func ReleaseAPIBatchService(app *appPkg.App, ctx context.Context) error {
	APIBatchServices.Lock()
	service := APIBatchServices.ByApp[app]
	if service == nil {
		APIBatchServices.Unlock()
		return nil
	}
	service.refs--
	last := service.refs <= 0
	if last {
		delete(APIBatchServices.ByApp, app)
	}
	APIBatchServices.Unlock()
	if !last {
		return nil
	}
	return service.stop(ctx)
}

func (s *APIBatchService) tokenLock(tokenID string) *sync.Mutex {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	lock := s.Locks[tokenID]
	if lock == nil {
		lock = &sync.Mutex{}
		s.Locks[tokenID] = lock
	}
	return lock
}

// ServiceSessionID is the id of the token's service session: api-<token id>-<h>
// where h is a fixed hash of the token id, so the id is stable across restarts.
func ServiceSessionID(tokenID string) string {
	sum := sha256.Sum256([]byte("api-batch-service:" + tokenID))
	return "api-" + tokenID + "-" + hex.EncodeToString(sum[:4])
}

// ensureServiceSession creates the token's service session on first use and
// refuses an existing session that is not one.
func (s *APIBatchService) ensureServiceSession(ctx context.Context, tokenID string) (string, error) {
	if s.app == nil || s.app.Sessions == nil {
		return "", errBatchUnavailable
	}
	id := ServiceSessionID(tokenID)
	item, err := s.app.Sessions.Get(ctx, id)
	if err == nil {
		if item.Origin != message.OriginAPIService {
			return "", errBatchServiceOwner
		}
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	creator, ok := s.app.Sessions.(coresession.OriginCreator)
	if !ok {
		return "", errBatchUnavailable
	}
	if _, err = creator.CreateWithIDAndOrigin(ctx, id, "API batches", message.OriginAPIService); err != nil {
		// A concurrent first use may have created it already.
		if again, getErr := s.app.Sessions.Get(ctx, id); getErr == nil && again.Origin == message.OriginAPIService {
			return id, nil
		}
		return "", err
	}
	return id, nil
}

// batchIntakePolicyFor derives the token's intake policy from its binding and mode.
func batchIntakePolicyFor(principal Principal) BatchIntakePolicy {
	policy := BatchIntakePolicy{ReadOnly: principal.Permission == "readonly"}
	if binding := principal.BatchBinding; binding != nil {
		policy.Tools = append([]string(nil), binding.Tools...)
		policy.MaxParallel = binding.MaxParallel
	}
	return policy
}

func batchMaxRoots(principal Principal) int {
	if binding := principal.BatchBinding; binding != nil && binding.MaxRoots > 0 {
		return binding.MaxRoots
	}
	return batchIntakeDefaultMaxRoots
}

func hashShort(value string, n int) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:n]
}

// batchRef builds the submit reference that carries the idempotency key and the
// body hash; it keeps the batch-cli: family so API roots never wake the owner.
func batchRef(tokenID, key, bodyHash string) func(root string) string {
	keyHash := batchNoKey
	if key != "" {
		keyHash = hashShort(key, 16)
	}
	return func(root string) string {
		return batchRefPrefix + tokenID + ":" + keyHash + ":" + bodyHash + ":" + root
	}
}

// parseBatchRef splits a submit reference of an API root.
func parseBatchRef(tokenID, ref string) (keyHash, bodyHash string, ok bool) {
	rest, found := strings.CutPrefix(ref, batchRefPrefix+tokenID+":")
	if !found {
		return "", "", false
	}
	parts := strings.Split(rest, ":")
	if len(parts) != 3 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

type apiBatchSubmitResult struct {
	Root   string
	Replay bool
}

// submit admits one request. The whole decision (idempotent replay, live-root
// limit, leaf budget, submission) runs under the token's lock so concurrent
// requests of one token cannot exceed max_roots or duplicate a key.
func (s *APIBatchService) submit(ctx context.Context, principal Principal, spec jobbatch.Spec, key, body string) (apiBatchSubmitResult, error) {
	if s.app == nil || !s.app.BatchOwnerRuntime() {
		return apiBatchSubmitResult{}, errBatchUnavailable
	}
	policy := batchIntakePolicyFor(principal)
	normalized, err := policy.Normalize(spec)
	if err != nil {
		return apiBatchSubmitResult{}, &batchInvalidError{err}
	}
	// Resolve every selected custom role against this App's pinned coordinator
	// snapshot before service-session creation, reservations, or submission.
	normalized, err = agent.AdmitBatchRoleContracts(ctx, s.app.AgentCoordinator, normalized)
	if err != nil {
		return apiBatchSubmitResult{}, &batchInvalidError{err}
	}
	ctx = agent.WithAdmittedBatchRoleContracts(ctx)
	lock := s.tokenLock(principal.TokenID)
	lock.Lock()
	defer lock.Unlock()
	owner, err := s.ensureServiceSession(ctx, principal.TokenID)
	if err != nil {
		return apiBatchSubmitResult{}, err
	}
	if err = s.precheckResume(ctx, owner, normalized); err != nil {
		return apiBatchSubmitResult{}, err
	}
	fac, err := s.app.BatchCLIStore()
	if err != nil {
		return apiBatchSubmitResult{}, err
	}
	roots, err := fac.Store.BatchAPIRoots(ctx, owner)
	if err != nil {
		return apiBatchSubmitResult{}, err
	}
	bodyHash := hashShort(body+agent.BatchRoleSnapshotHash(normalized), 16)
	if key != "" {
		keyHash := hashShort(key, 16)
		for _, root := range roots {
			existingKey, existingBody, ok := parseBatchRef(principal.TokenID, root.Ref)
			if !ok || existingKey != keyHash {
				continue
			}
			if existingBody != bodyHash {
				return apiBatchSubmitResult{}, errBatchKeyConflict
			}
			return apiBatchSubmitResult{Root: root.Root, Replay: true}, nil
		}
	}
	live := 0
	for _, root := range roots {
		if session.BatchAPIRootLive(root.State) {
			live++
		}
	}
	if live >= batchMaxRoots(principal) {
		return apiBatchSubmitResult{}, fmt.Errorf("%w: %d live roots", errBatchOverloaded, live)
	}
	parallel := policy.maxParallel()
	reservation, ok := s.reserve(parallel)
	if !ok {
		return apiBatchSubmitResult{}, fmt.Errorf("%w: engine leaf budget", errBatchOverloaded)
	}
	if principal.Permission == "auto" {
		s.app.BatchAutoApproveOwner(owner)
	}
	// The served intake carries the answer endpoint, so its roots keep working held questions.
	root, err := s.app.BatchSubmitRefWithQuestionRoute(ctx, owner, normalized, batchRef(principal.TokenID, key, bodyHash), true)
	if err != nil {
		s.releaseReservation(reservation)
		return apiBatchSubmitResult{}, err
	}
	s.commitReservation(reservation, root)
	s.watch(owner, root)
	return apiBatchSubmitResult{Root: root}, nil
}

// reserve takes parallel leaves from the process-wide API budget under a
// temporary key; commit renames it to the root id, release gives it back.
func (s *APIBatchService) reserve(parallel int) (string, bool) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	used := 0
	for _, value := range s.Watching {
		used += value
	}
	if used+parallel > batchIntakeLeafBudget {
		return "", false
	}
	key := "reserved:" + uuid.NewString()
	s.Watching[key] = parallel
	return key, true
}

func (s *APIBatchService) commitReservation(key, root string) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.Watching[root] = s.Watching[key]
	delete(s.Watching, key)
}

func (s *APIBatchService) releaseReservation(key string) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	delete(s.Watching, key)
}

// watch pumps stop requests until the root is terminal, settles the notice debt
// at once, keeps the live output readable for outputTTL, then releases the root.
// It runs whether or not any client ever polls.
func (s *APIBatchService) watch(owner, root string) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.Mu.Lock()
			delete(s.Watching, root)
			s.Mu.Unlock()
		}()
		if _, err := s.app.BatchWait(s.ctx, owner, root); err != nil {
			if s.ctx.Err() == nil {
				slog.Warn("api batch watcher: wait failed", "root", root, "error", err)
			}
			return
		}
		if err := s.app.BatchSettleDebt(s.ctx, owner, root); err != nil && s.ctx.Err() == nil {
			slog.Warn("api batch watcher: settle debt failed", "root", root, "error", err)
		}
		timer := time.NewTimer(s.OutputTTL)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-s.ctx.Done():
			return
		}
		if err := s.app.BatchRelease(s.ctx, owner, root); err != nil && s.ctx.Err() == nil {
			slog.Warn("api batch watcher: release failed", "root", root, "error", err)
		}
	}()
}

type batchInvalidError struct{ cause error }

func (e *batchInvalidError) Error() string { return e.cause.Error() }
func (e *batchInvalidError) Unwrap() error { return e.cause }

// apiBatchView is the durable read of one root plus best-effort live output.
type apiBatchView struct {
	Root    db.BatchNode
	Nodes   []db.BatchNode
	Outputs map[string]string
	// Held maps a node id to the unanswered question of its child.
	Held map[string]string
	Live bool
}

// read returns the root of the token's own service session. A root of another
// token (or a missing one) is the same errBatchNotFound.
func (s *APIBatchService) read(ctx context.Context, tokenID, root string) (apiBatchView, error) {
	if s.app == nil {
		return apiBatchView{}, errBatchUnavailable
	}
	owner := ServiceSessionID(tokenID)
	fac, err := s.app.BatchCLIStore()
	if err != nil {
		return apiBatchView{}, err
	}
	s.Mu.Lock()
	_, live := s.Watching[root]
	s.Mu.Unlock()
	if !live {
		// The periodic recovery pass can lag; a read settles dead hosts first.
		if _, recoverErr := fac.Store.BatchRecoverOwnerScope(ctx, owner, s.hostProbe()); recoverErr != nil {
			slog.Debug("api batch read: owner recovery", "error", recoverErr)
		}
	}
	snapshot, err := fac.Store.Read(ctx, owner, root)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return apiBatchView{}, errBatchNotFound
		}
		return apiBatchView{}, err
	}
	view := apiBatchView{Root: snapshot.Root, Nodes: snapshot.Nodes, Live: live}
	if live {
		view.Outputs = s.liveOutputs(ctx, owner, root, snapshot.Nodes)
	}
	if held, heldErr := fac.Store.BatchAPIHeldQuestions(ctx, owner); heldErr == nil {
		view.Held = make(map[string]string, len(held))
		for _, question := range held {
			if s.questionAnswered(question) {
				continue
			}
			view.Held[question.Node] = question.Question
		}
	}
	return view, nil
}

// liveOutputs collects the best-effort live output of the finished tasks. A node
// that is still running is skipped (its output call would block until it ends),
// and each call is bounded, so a read never hangs on a live root.
func (s *APIBatchService) liveOutputs(ctx context.Context, owner, root string, nodes []db.BatchNode) map[string]string {
	outputs := make(map[string]string)
	for _, node := range nodes {
		if node.Kind != "task" || !batchStateDone(node.State) {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, time.Second)
		text, err := s.app.BatchNodeOutput(callCtx, owner, root, node.NodeID)
		cancel()
		if err == nil && text != "" {
			outputs[node.NodeID] = text
		}
	}
	return outputs
}

// stopRequest asks the owning process (this one, via the watcher's pump) to stop
// the root, or one node of it.
func (s *APIBatchService) stopRequest(ctx context.Context, tokenID, root, node string) (db.BatchControlRequest, error) {
	view, err := s.read(ctx, tokenID, root)
	if err != nil {
		return db.BatchControlRequest{}, err
	}
	if node == "" {
		node = root
	}
	found := false
	for _, candidate := range view.Nodes {
		found = found || candidate.NodeID == node
	}
	if !found {
		return db.BatchControlRequest{}, errBatchNotFound
	}
	fac, err := s.app.BatchCLIStore()
	if err != nil {
		return db.BatchControlRequest{}, err
	}
	target := session.BatchControlTarget{Owner: ServiceSessionID(tokenID), Host: view.Root.HostID, Root: root, Node: node}
	return fac.Store.BatchRequestStop(ctx, target, s.hostProbe())
}

// precheckResume gives an early, readable error for an `agent` item that resumes
// a session which is not a direct child of the token's service session. The
// coordinator enforces the same rule twice more (resume parent check and the
// durable child bind); this only avoids a late opaque leaf failure.
func (s *APIBatchService) precheckResume(ctx context.Context, owner string, spec jobbatch.Spec) error {
	if spec.Task != nil {
		var task agent.BatchCLITask
		if json.Unmarshal(spec.Task.Payload, &task) != nil || task.Tool != agentToolName {
			return nil
		}
		var input struct {
			Resume string `json:"resume_session_id"`
		}
		if json.Unmarshal(task.Input, &input) != nil || input.Resume == "" {
			return nil
		}
		child, err := s.app.Sessions.Get(ctx, input.Resume)
		if err != nil || child.ParentSessionID != owner {
			return &batchInvalidError{fmt.Errorf("resume_session_id %q is not a child of this token's batches", input.Resume)}
		}
		return nil
	}
	if spec.Batch != nil {
		for _, item := range spec.Batch.Items {
			if err := s.precheckResume(ctx, owner, item); err != nil {
				return err
			}
		}
	}
	return nil
}

// hostProbe answers "is this batch host alive" for stop requests and owner-scope
// recovery: this process's own hosts are alive by definition (the shared probe
// refuses to probe them), other hosts are probed through their lock files.
func (s *APIBatchService) hostProbe() session.BatchHostProbe {
	shared := session.BatchSharedHostProbe(s.app.DataDirectory())
	return func(host string) (sessionhosts.HostLockStatus, error) {
		if sessionhosts.IsOwnHostID(host) {
			return session.HostStatusAlive, nil
		}
		return shared(host)
	}
}

// answerHeld answers the held question of a node of the token's own root, in the
// same execution. errBatchNoQuestion covers "no question" and "stale question".
func (s *APIBatchService) answerHeld(ctx context.Context, tokenID, root, node, text string) error {
	view, err := s.read(ctx, tokenID, root)
	if err != nil {
		return err
	}
	var child string
	found := false
	for _, candidate := range view.Nodes {
		if candidate.NodeID == node {
			found = true
			if candidate.ChildSessionID.Valid {
				child = candidate.ChildSessionID.String
			}
		}
	}
	if !found {
		return errBatchNotFound
	}
	if child == "" {
		return errBatchNoQuestion
	}
	var pending []session.BatchAPIHeldQuestion
	if fac, facErr := s.app.BatchCLIStore(); facErr == nil {
		if held, heldErr := fac.Store.BatchAPIHeldQuestions(ctx, ServiceSessionID(tokenID)); heldErr == nil {
			for _, question := range held {
				if question.Node == node {
					pending = append(pending, question)
				}
			}
		}
	}
	err = s.app.BatchAnswerHeld(ctx, ServiceSessionID(tokenID), child, text)
	switch {
	case err == nil:
		s.markAnswered(pending)
		return nil
	case errors.Is(err, agent.ErrBatchHeldAnswerNone), errors.Is(err, agent.ErrBatchAgentStaleGeneration), errors.Is(err, agent.ErrBatchAgentStopped), errors.Is(err, agent.ErrBatchAgentNotHeld), errors.Is(err, jobbatch.ErrReleased):
		return fmt.Errorf("%w: %v", errBatchNoQuestion, err)
	default:
		return err
	}
}

func answeredKey(question session.BatchAPIHeldQuestion) string {
	return fmt.Sprintf("%s/%d/%d", question.Node, question.Token, question.Sequence)
}

func (s *APIBatchService) markAnswered(questions []session.BatchAPIHeldQuestion) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	for _, question := range questions {
		s.Answered[answeredKey(question)] = struct{}{}
	}
}

func (s *APIBatchService) questionAnswered(question session.BatchAPIHeldQuestion) bool {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	_, ok := s.Answered[answeredKey(question)]
	return ok
}

// WiredRoutes is the exported entry point of the wired batch route table;
// the composer's route builder calls it (design §3 S6).
func WiredRoutes(a *appPkg.App) []APIRoute {
	return retainAPIBatchService(a).apiBatchRoutes()
}
