// yeet is a thin frontend + orchestrator over the Coolify API. It doesn't
// reimplement deployment logic - every deploy path maps to one existing
// Coolify endpoint (applications/private-github-app, applications/public,
// applications/dockerfile, or services with docker_compose_raw).
package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/jcsawyer123/yeet/internal/coolify"
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
	ListenAddr      string
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
		ListenAddr:      ":" + envOr("PORT", "7000"),
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

type server struct {
	cfg    config
	client *coolify.Client
	tmpl   *template.Template
}

func main() {
	cfg := loadConfig()
	s := &server{
		cfg:    cfg,
		client: coolify.NewClient(cfg.CoolifyBaseURL, cfg.CoolifyToken),
		tmpl:   template.Must(template.ParseFS(webFS, "web/*.html")),
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
	Type       string `json:"type"` // "github" | "public" | "dockerfile" | "compose"
	Repository string `json:"repository,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Port       string `json:"port,omitempty"`
	Dockerfile string `json:"dockerfile,omitempty"`
	Compose    string `json:"compose,omitempty"`
	Name       string `json:"name,omitempty"`
	BuildPack  string `json:"build_pack,omitempty"` // for github/public: nixpacks|dockerfile|dockercompose
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
	description := "yeet: " + name

	switch req.Type {
	case "github":
		res, err := s.client.CreatePrivateGithubApp(coolify.PrivateGithubAppRequest{
			ProjectUUID:         s.cfg.ProjectUUID,
			ServerUUID:          s.cfg.ServerUUID,
			EnvironmentName:     s.cfg.EnvironmentName,
			GithubAppUUID:       s.cfg.GithubAppUUID,
			GitRepository:       req.Repository,
			GitBranch:           branch,
			BuildPack:           buildPack,
			PortsExposes:        req.Port,
			Name:                name,
			Description:         description,
			Domains:             domain,
			IsAutoDeployEnabled: true,
		})
		s.finishDeploy(w, res, "application", domain, err, func() error { return s.client.DeployApplication(res.UUID) })

	case "public":
		res, err := s.client.CreatePublicRepo(coolify.PublicRepoRequest{
			ProjectUUID:     s.cfg.ProjectUUID,
			ServerUUID:      s.cfg.ServerUUID,
			EnvironmentName: s.cfg.EnvironmentName,
			GitRepository:   req.Repository,
			GitBranch:       branch,
			BuildPack:       buildPack,
			PortsExposes:    req.Port,
			Name:            name,
			Description:     description,
			Domains:         domain,
		})
		s.finishDeploy(w, res, "application", domain, err, func() error { return s.client.DeployApplication(res.UUID) })

	case "dockerfile":
		res, err := s.client.CreateDockerfile(coolify.DockerfileRequest{
			ProjectUUID:     s.cfg.ProjectUUID,
			ServerUUID:      s.cfg.ServerUUID,
			EnvironmentName: s.cfg.EnvironmentName,
			Dockerfile:      req.Dockerfile,
			BuildPack:       "dockerfile",
			PortsExposes:    req.Port,
			Name:            name,
			Description:     description,
			Domains:         domain,
		})
		s.finishDeploy(w, res, "application", domain, err, func() error { return s.client.DeployApplication(res.UUID) })

	case "compose":
		res, err := s.client.CreateService(coolify.ServiceRequest{
			ProjectUUID:      s.cfg.ProjectUUID,
			ServerUUID:       s.cfg.ServerUUID,
			EnvironmentName:  s.cfg.EnvironmentName,
			Name:             name,
			Description:      description,
			DockerComposeRaw: req.Compose,
			InstantDeploy:    true,
		})
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, deployResponse{URL: domain, UUID: res.UUID, Kind: "service"})

	default:
		writeErr(w, http.StatusBadRequest, fmt.Errorf("unknown type %q (want github, public, dockerfile, or compose)", req.Type))
	}
}

func (s *server) finishDeploy(w http.ResponseWriter, res *coolify.CreateResult, kind, domain string, err error, deploy func() error) {
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if err := deploy(); err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("created but failed to deploy: %w", err))
		return
	}
	writeJSON(w, deployResponse{URL: domain, UUID: res.UUID, Kind: kind})
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

	type item struct {
		UUID        string `json:"uuid"`
		Name        string `json:"name"`
		Kind        string `json:"kind"`
		Description string `json:"description"`
	}
	var out []item
	for _, a := range apps {
		if strings.HasPrefix(a.Description, "yeet:") {
			out = append(out, item{a.UUID, a.Name, "application", a.Description})
		}
	}
	for _, sv := range services {
		if strings.HasPrefix(sv.Description, "yeet:") {
			out = append(out, item{sv.UUID, sv.Name, "service", sv.Description})
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
