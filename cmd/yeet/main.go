// yeet is a thin frontend + orchestrator over the Coolify API. It doesn't
// reimplement deployment logic - every deploy path maps to one existing
// Coolify endpoint (applications/private-github-app, applications/public,
// applications/dockerfile, or services with docker_compose_raw).
package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jcsawyer123/yeet/internal/coolify"
	"github.com/jcsawyer123/yeet/internal/reconcile"
	"github.com/jcsawyer123/yeet/internal/store"
)

//go:embed web/*.html
var webFS embed.FS

type config struct {
	CoolifyBaseURL  string
	CoolifyToken    string
	ProjectUUID     string
	ServerUUID      string
	EnvironmentName string
	GithubAppUUID   string
	BaseDomain      string
	DashboardURL    string
	ListenAddr      string
	SelfUUID        string
	DBPath          string
}

func loadConfig() config {
	cfg := config{
		CoolifyBaseURL:  os.Getenv("COOLIFY_BASE_URL"),
		CoolifyToken:    os.Getenv("COOLIFY_API_TOKEN"),
		ProjectUUID:     os.Getenv("COOLIFY_PROJECT_UUID"),
		ServerUUID:      os.Getenv("COOLIFY_SERVER_UUID"),
		EnvironmentName: os.Getenv("COOLIFY_ENVIRONMENT_NAME"),
		GithubAppUUID:   os.Getenv("COOLIFY_GITHUB_APP_UUID"),
		BaseDomain:      os.Getenv("BASE_DOMAIN"),
		DashboardURL:    strings.TrimSuffix(envOr("COOLIFY_DASHBOARD_URL", "https://coolify.home.jcsx.me"), "/"),
		ListenAddr:      ":" + envOr("PORT", "7000"),
		// Coolify injects this into every container it deploys, including
		// yeet's own - used to exclude yeet from its own deployment list so
		// you can't stop/delete the tool out from under yourself. Empty in
		// local dev, which is fine: nothing to exclude.
		SelfUUID: os.Getenv("COOLIFY_RESOURCE_UUID"),
		// Should point at a Coolify persistent volume in production -
		// otherwise every redeploy wipes yeet's local state. Defaults to a
		// relative path for local dev, where that doesn't matter.
		DBPath: envOr("YEET_DB_PATH", "yeet.db"),
	}
	if cfg.EnvironmentName == "" {
		cfg.EnvironmentName = "production"
	}
	missing := []string{}
	for name, val := range map[string]string{
		"COOLIFY_BASE_URL":     cfg.CoolifyBaseURL,
		"COOLIFY_API_TOKEN":    cfg.CoolifyToken,
		"COOLIFY_PROJECT_UUID": cfg.ProjectUUID,
		"COOLIFY_SERVER_UUID":  cfg.ServerUUID,
		"BASE_DOMAIN":          cfg.BaseDomain,
	} {
		if val == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		log.Fatalf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	return cfg
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Coolify hardcodes git_repository to this placeholder for every
// dockerfile-type app (it's not a real repo) - don't surface it as if it
// were the app's source.
func displayGitRepository(repo string) string {
	if repo == "coollabsio/coolify" {
		return ""
	}
	return repo
}

type server struct {
	cfg             config
	client          *coolify.Client
	tmpl            *template.Template
	environmentUUID string    // resolved best-effort at startup, used only for dashboard deep links
	db              *store.DB // nil if the database couldn't be opened - policy features degrade to no-ops
}

func main() {
	cfg := loadConfig()
	s := &server{
		cfg:    cfg,
		client: coolify.NewClient(cfg.CoolifyBaseURL, cfg.CoolifyToken),
		tmpl:   template.Must(template.ParseFS(webFS, "web/*.html")),
	}
	s.resolveEnvironmentUUID()

	// Non-fatal: yeet's core deploy/list/manage flow doesn't depend on the
	// database (a deploy still works without TTL/reset policy tracking),
	// so a DB problem shouldn't take the whole tool down.
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Printf("warning: could not open database at %q, policy tracking disabled: %v", cfg.DBPath, err)
	} else {
		s.db = db
		defer db.Close()
		go reconcile.New(s.client, db, cfg.SelfUUID, cfg.ProjectUUID, cfg.ServerUUID, cfg.EnvironmentName, cfg.GithubAppUUID).
			Run(context.Background(), 30*time.Second)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("POST /api/deploy", s.handleDeploy)
	mux.HandleFunc("GET /api/list", s.handleList)
	mux.HandleFunc("POST /api/apps/{uuid}/stop", s.handleAppAction("stop"))
	mux.HandleFunc("POST /api/apps/{uuid}/start", s.handleAppAction("start"))
	mux.HandleFunc("DELETE /api/apps/{uuid}", s.handleAppAction("delete"))
	mux.HandleFunc("POST /api/services/{uuid}/stop", s.handleServiceAction("stop"))
	mux.HandleFunc("POST /api/services/{uuid}/start", s.handleServiceAction("start"))
	mux.HandleFunc("DELETE /api/services/{uuid}", s.handleServiceAction("delete"))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("yeet listening on %s", cfg.ListenAddr)
	log.Fatal(http.ListenAndServe(cfg.ListenAddr, mux))
}

// resolveEnvironmentUUID looks up the environment's uuid once at startup.
// Coolify's dashboard URLs are keyed by environment uuid, not the
// environment_name used everywhere else in this app's config. Best-effort:
// dashboard links just degrade to the project overview page if this fails.
func (s *server) resolveEnvironmentUUID() {
	envs, err := s.client.ListEnvironments(s.cfg.ProjectUUID)
	if err != nil {
		log.Printf("warning: could not resolve environment uuid, dashboard links will be less specific: %v", err)
		return
	}
	for _, e := range envs {
		if e.Name == s.cfg.EnvironmentName {
			s.environmentUUID = e.UUID
			return
		}
	}
	log.Printf("warning: no environment named %q found, dashboard links will be less specific", s.cfg.EnvironmentName)
}

func (s *server) dashboardLink(kind, uuid string) string {
	if s.cfg.DashboardURL == "" {
		return ""
	}
	if s.environmentUUID == "" {
		return s.cfg.DashboardURL + "/project/" + s.cfg.ProjectUUID
	}
	return fmt.Sprintf("%s/project/%s/environment/%s/%s/%s", s.cfg.DashboardURL, s.cfg.ProjectUUID, s.environmentUUID, kind, uuid)
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	who := r.Header.Get("X-Authentik-Username")
	if who == "" {
		who = "there"
	}
	if err := s.tmpl.ExecuteTemplate(w, "index.html", map[string]any{
		"Who":        who,
		"BaseDomain": s.cfg.BaseDomain,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type deployRequest struct {
	Type               string `json:"type"` // "github" | "public" | "dockerfile" | "compose"
	Repository         string `json:"repository,omitempty"`
	Branch             string `json:"branch,omitempty"`
	Port               string `json:"port,omitempty"`
	Dockerfile         string `json:"dockerfile,omitempty"`
	Compose            string `json:"compose,omitempty"`
	Name               string `json:"name,omitempty"`
	BuildPack          string `json:"build_pack,omitempty"` // for github/public: nixpacks|dockerfile|dockercompose
	HealthCheckEnabled bool   `json:"health_check_enabled,omitempty"`
	HealthCheckPath    string `json:"health_check_path,omitempty"`
	// Policy (Phase 2): optional. Not supported for "compose" deploys yet -
	// reset-via-recreate needs a single stable dispatch path per source
	// type, and services don't need it as urgently as ad-hoc apps do.
	TTLSeconds           *int64 `json:"ttl_seconds,omitempty"`
	ResetIntervalSeconds *int64 `json:"reset_interval_seconds,omitempty"`
	ExpiryAction         string `json:"expiry_action,omitempty"` // "stop" | "delete", default "stop"
}

type deployResponse struct {
	URL  string `json:"url"`
	UUID string `json:"uuid"`
	Kind string `json:"kind"` // "application" | "service"
}

func (s *server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	var req deployRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Type == "compose" && (req.TTLSeconds != nil || req.ResetIntervalSeconds != nil) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("ttl/reset policy isn't supported for compose deploys yet"))
		return
	}

	name := req.Name
	if name == "" {
		name = coolify.RandomName()
	}
	domain := "https://" + name + "." + s.cfg.BaseDomain
	branch := req.Branch
	if branch == "" {
		branch = "main"
	}
	buildPack := req.BuildPack
	if buildPack == "" {
		buildPack = "dockerfile"
	}

	spec := coolify.DeploySpec{
		SourceType:         req.Type,
		ProjectUUID:        s.cfg.ProjectUUID,
		ServerUUID:         s.cfg.ServerUUID,
		EnvironmentName:    s.cfg.EnvironmentName,
		GithubAppUUID:      s.cfg.GithubAppUUID,
		GitRepository:      req.Repository,
		GitBranch:          branch,
		BuildPack:          buildPack,
		Dockerfile:         req.Dockerfile,
		Compose:            req.Compose,
		PortsExposes:       req.Port,
		Name:               name,
		Description:        "yeet: " + name,
		Domains:            domain,
		HealthCheckEnabled: req.HealthCheckEnabled,
		HealthCheckPath:    req.HealthCheckPath,
	}

	result, err := s.client.CreateFromSpec(spec)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	if req.TTLSeconds != nil || req.ResetIntervalSeconds != nil {
		if err := s.registerPolicy(spec, result, domain, req.TTLSeconds, req.ResetIntervalSeconds, req.ExpiryAction); err != nil {
			// The deploy itself succeeded - a policy bookkeeping failure
			// shouldn't fail the whole request, just means no TTL/reset
			// will be enforced for it. Log so it's not silently lost.
			log.Printf("register policy for %s: %v", result.UUID, err)
		}
	}

	writeJSON(w, deployResponse{URL: domain, UUID: result.UUID, Kind: result.Kind})
}

// registerPolicy records a project+instance with TTL/reset policy for a
// deploy that just succeeded. A no-op if the database isn't available.
func (s *server) registerPolicy(spec coolify.DeploySpec, result *coolify.DeployResult, domain string, ttl, resetInterval *int64, expiryAction string) error {
	if s.db == nil {
		return fmt.Errorf("database not available")
	}
	project, err := s.db.CreateProjectWithSpec(store.ProjectSpec{
		Name:                 spec.Name,
		SourceType:           spec.SourceType,
		GitRepository:        spec.GitRepository,
		GitBranch:            spec.GitBranch,
		BuildPack:            spec.BuildPack,
		DockerfileBlob:       spec.Dockerfile,
		ComposeBlob:          spec.Compose,
		PortsExposes:         spec.PortsExposes,
		TTLSeconds:           ttl,
		ResetIntervalSeconds: resetInterval,
		ExpiryAction:         expiryAction,
	})
	if err != nil {
		return err
	}
	if err := s.db.CreateInstance(project.ID, spec.Name, result.UUID, result.Kind, ttl, resetInterval); err != nil {
		return err
	}
	return s.db.UpdateInstanceObserved(result.UUID, "", domain, time.Now())
}

func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	apps, err := s.client.ListApplications()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	services, err := s.client.ListServices()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	var policies map[string]store.InstancePolicy
	if s.db != nil {
		policies, err = s.db.ListInstancePolicies()
		if err != nil {
			log.Printf("list instance policies: %v", err)
		}
	}

	type item struct {
		UUID          string     `json:"uuid"`
		Name          string     `json:"name"`
		Kind          string     `json:"kind"`
		Description   string     `json:"description"`
		URL           string     `json:"url,omitempty"`
		Status        string     `json:"status,omitempty"`
		GitRepository string     `json:"git_repository,omitempty"`
		DashboardURL  string     `json:"dashboard_url,omitempty"`
		ExpiresAt     *time.Time `json:"expires_at,omitempty"`
		NextResetAt   *time.Time `json:"next_reset_at,omitempty"`
	}
	var out []item
	for _, a := range apps {
		if a.UUID == s.cfg.SelfUUID {
			continue
		}
		if strings.HasPrefix(a.Description, "yeet:") {
			p := policies[a.UUID]
			out = append(out, item{
				UUID:          a.UUID,
				Name:          a.Name,
				Kind:          "application",
				Description:   a.Description,
				URL:           a.FQDN,
				Status:        a.Status,
				GitRepository: displayGitRepository(a.GitRepository),
				DashboardURL:  s.dashboardLink("application", a.UUID),
				ExpiresAt:     p.ExpiresAt,
				NextResetAt:   p.NextResetAt,
			})
		}
	}
	for _, sv := range services {
		if sv.UUID == s.cfg.SelfUUID {
			continue
		}
		if strings.HasPrefix(sv.Description, "yeet:") {
			p := policies[sv.UUID]
			out = append(out, item{
				UUID:         sv.UUID,
				Name:         sv.Name,
				Kind:         "service",
				Description:  sv.Description,
				Status:       sv.Status,
				DashboardURL: s.dashboardLink("service", sv.UUID),
				ExpiresAt:    p.ExpiresAt,
				NextResetAt:  p.NextResetAt,
			})
		}
	}
	writeJSON(w, out)
}

func (s *server) handleAppAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uuid := r.PathValue("uuid")
		var err error
		switch action {
		case "stop":
			err = s.client.StopApplication(uuid)
		case "start":
			err = s.client.StartApplication(uuid)
		case "delete":
			err = s.client.DeleteApplication(uuid)
		}
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func (s *server) handleServiceAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uuid := r.PathValue("uuid")
		var err error
		switch action {
		case "stop":
			err = s.client.StopService(uuid)
		case "start":
			err = s.client.StartService(uuid)
		case "delete":
			err = s.client.DeleteService(uuid)
		}
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
