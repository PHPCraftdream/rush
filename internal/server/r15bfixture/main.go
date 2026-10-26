package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/server"
	"github.com/PHPCraftdream/rush/internal/session"
)

type fixtureState struct {
	Owner          string   `json:"owner"`
	Root           string   `json:"root"`
	GateDir        string   `json:"gateDir"`
	Token          string   `json:"token"`
	Children       []string `json:"children"`
	NoticeNode     string   `json:"noticeNode"`
	SuppressedNode string   `json:"suppressedNode"`
}

var (
	mu                          sync.Mutex
	owner, rootID, gates, token string
	children                    []string
	noticeNode, suppressedNode  string
	noticesSet                  bool
)

func main() {
	if os.Getenv("RUSH_R15B_REAL_BACKEND") != "1" {
		fmt.Fprintln(os.Stderr, "set RUSH_R15B_REAL_BACKEND=1 to run fixture")
		os.Exit(2)
	}
	if len(os.Args) > 1 && os.Args[1] == "--r15b-child" {
		child()
		return
	}
	temp, err := os.MkdirTemp("", "rush-r15b-")
	must(err)
	defer os.RemoveAll(temp)
	data, cfg := filepath.Join(temp, "data"), filepath.Join(temp, "config")
	for _, d := range []string{data, cfg} {
		must(os.MkdirAll(d, 0o700))
	}
	for k, v := range map[string]string{"RUSH_GLOBAL_DATA": data, "RUSH_GLOBAL_CONFIG": cfg, "XDG_DATA_HOME": data, "XDG_CONFIG_HOME": cfg, "RUSH_PROVIDER_CACHE_ONLY": "1", "RUSH_HEARTBEAT_DIR": ""} {
		must(os.Setenv(k, v))
	}
	provider := &fakeLLM{}
	provider.URL = "http://127.0.0.1:60987"
	ps := &http.Server{Addr: "127.0.0.1:60987", Handler: provider}
	go func() { _ = ps.ListenAndServe() }()
	defer ps.Shutdown(context.Background())
	store, err := config.Init(temp, data, false)
	must(err)
	store.Config().Providers.Set("openaicompat", config.ProviderConfig{ID: "openaicompat", Type: openaicompat.Name, BaseURL: provider.URL, APIKey: "fixture", Models: []catwalk.Model{{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1024}}})
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
	store.SetupAgents()
	conn, err := db.Connect(context.Background(), data)
	must(err)
	a, err := app.New(context.Background(), conn, store)
	must(err)
	defer a.Shutdown()
	gates = filepath.Join(temp, "gates")
	must(os.MkdirAll(gates, 0o700))
	mux := http.NewServeMux()
	mux.HandleFunc("/fixture/reset", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		reset(w, r, a)
	})
	mux.HandleFunc("/fixture/state", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(fixtureState{owner, rootID, gates, token, children, noticeNode, suppressedNode})
	})
	mux.HandleFunc("/fixture/release/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/fixture/release/")
		if strings.Contains(id, "..") || filepath.Base(id) != id {
			http.Error(w, "invalid child", 400)
			return
		}
		must(os.WriteFile(filepath.Join(gates, id), []byte("go"), 0o600))
		w.WriteHeader(204)
	})
	mux.HandleFunc("/fixture/notices", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		notices(w, r, a)
	})
	mux.HandleFunc("/fixture/auth", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
	})
	controller := &http.Server{Addr: "127.0.0.1:60986", Handler: mux}
	go func() { _ = controller.ListenAndServe() }()
	defer controller.Shutdown(context.Background())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dist := os.Getenv("RUSH_R15B_WEB_DIST")
	if dist == "" {
		dist = filepath.Join("web", "dist")
	}
	srv := server.New(a, "127.0.0.1:60985", os.DirFS(dist))
	token = srv.Token()
	if err := srv.Start(ctx, func(addr string) { fmt.Printf("R15B_FIXTURE_ADDR=http://%s\n", addr) }); err != nil {
		slog.Error("fixture server failed", "error", err)
		os.Exit(1)
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	must(err)
	return json.RawMessage(b)
}

func reset(w http.ResponseWriter, r *http.Request, a *app.App) {
	mu.Lock()
	defer mu.Unlock()
	if rootID != "" {
		http.Error(w, "fixture batch already submitted", 409)
		return
	}
	sess, err := a.Sessions.Create(r.Context(), "r15b fixture")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	owner = sess.ID
	bin, err := os.Executable()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	children = []string{"1.1", "1.2", "2", "3"}
	task := func(node string) jobbatch.Spec {
		input := mustJSON(map[string]any{"program": bin, "args": []string{"--r15b-child", gates, node}, "description": "fixture " + node, "timeout_seconds": 600})
		payload, _ := json.Marshal(map[string]any{"tool": "run_command", "input": input})
		return jobbatch.Spec{Task: &jobbatch.TaskSpec{Kind: "generic", Payload: payload}}
	}
	spec := jobbatch.Spec{Batch: &jobbatch.BatchSpec{Mode: jobbatch.ModeParallel, Notify: jobbatch.NotifyEach, Items: []jobbatch.Spec{{Batch: &jobbatch.BatchSpec{Mode: jobbatch.ModeParallel, Items: []jobbatch.Spec{task("1.1"), task("1.2")}}}, task("2"), task("3")}}}
	id, err := a.BatchSubmit(r.Context(), owner, spec)
	if err != nil {
		owner = ""
		http.Error(w, err.Error(), 500)
		return
	}
	rootID = id
	noticeNode = rootID + ".2"
	suppressedNode = rootID + ".3"
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
		defer cancel()
		_, _ = a.BatchWait(ctx, owner, id)
	}()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"root": id, "owner": owner})
}

// notices makes delayed and suppressed batch notices observable through the
// PUBLIC session batch store projection: noticeNode (root.2) is completed with a
// pending selected notice (delayed), suppressedNode (root.3) is completed without a notice
// (suppressed, NoticeSelected stays 0).
func notices(w http.ResponseWriter, r *http.Request, a *app.App) {
	mu.Lock()
	defer mu.Unlock()
	if rootID == "" {
		http.Error(w, "fixture batch not submitted", 409)
		return
	}
	if noticesSet {
		http.Error(w, "fixture notices already projected", 409)
		return
	}
	ctx := r.Context()
	store := session.NewBatchStore(a.DB())
	host := a.BatchHostStore().HostID()
	project := func(id string, withNotice bool) error {
		node, err := store.BatchCLIGetNode(ctx, owner, rootID, id)
		if err != nil {
			return err
		}
		rootNode, err := store.BatchCLIGetNode(ctx, owner, rootID, rootID)
		if err != nil {
			return err
		}
		seq := max(rootNode.RootSequence, rootNode.ProjectionSequence, node.ProjectionSequence) + 1
		p := session.BatchProjection{Host: host, RunnerID: node.RunnerID, Sequence: seq, Roots: []session.BatchRootProjection{{Owner: owner, Root: rootID, Updates: []db.ProjectBatchNodeParams{{NodeID: id, State: "completed", ControlState: "none", OutcomeCode: "ok", OutcomeSummary: "fixture " + id}}}}}
		if withNotice {
			p.Roots[0].Notices = []db.SelectBatchNodeNoticeParams{{NodeID: id, NoticeKind: sql.NullString{String: "task", Valid: true}, NoticeState: sql.NullString{String: "pending", Valid: true}, NoticeCause: sql.NullString{String: "", Valid: true}, NoticeOrdinal: sql.NullInt64{Int64: 0, Valid: true}, NoticeCompleted: 1, Wake: 1}}
		}
		return store.Project(ctx, p)
	}
	if err := project(noticeNode, true); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := project(suppressedNode, false); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	noticesSet = true
	w.WriteHeader(204)
}

func child() {
	if len(os.Args) < 4 {
		os.Exit(2)
	}
	p := filepath.Join(os.Args[2], os.Args[3])
	for {
		if _, e := os.Stat(p); e == nil {
			fmt.Println("fixture child released")
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
