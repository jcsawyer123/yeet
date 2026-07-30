// Package coolify is a thin client for the subset of the Coolify REST API
// that yeet needs. It does not reimplement any deployment logic - every
// call maps directly to a Coolify endpoint.
package coolify

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) do(method, path string, body any, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("coolify api %s %s -> %d: %s", method, path, resp.StatusCode, string(respBody))
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode response: %w (body: %s)", err, string(respBody))
		}
	}
	return nil
}

// --- Applications (git/Dockerfile-backed) ---

type CreateResult struct {
	UUID    string `json:"uuid"`
	Domains string `json:"domains"`
}

type PrivateGithubAppRequest struct {
	ProjectUUID         string `json:"project_uuid"`
	ServerUUID          string `json:"server_uuid"`
	EnvironmentName     string `json:"environment_name"`
	GithubAppUUID       string `json:"github_app_uuid"`
	GitRepository       string `json:"git_repository"`
	GitBranch           string `json:"git_branch"`
	BuildPack           string `json:"build_pack"`
	PortsExposes        string `json:"ports_exposes,omitempty"`
	Name                string `json:"name,omitempty"`
	Description         string `json:"description,omitempty"`
	Domains             string `json:"domains,omitempty"`
	IsAutoDeployEnabled bool   `json:"is_auto_deploy_enabled"`
	HealthCheckEnabled  bool   `json:"health_check_enabled,omitempty"`
	HealthCheckPath     string `json:"health_check_path,omitempty"`
}

func (c *Client) CreatePrivateGithubApp(req PrivateGithubAppRequest) (*CreateResult, error) {
	var out CreateResult
	err := c.do(http.MethodPost, "/api/v1/applications/private-github-app", req, &out)
	return &out, err
}

type PublicRepoRequest struct {
	ProjectUUID        string `json:"project_uuid"`
	ServerUUID         string `json:"server_uuid"`
	EnvironmentName    string `json:"environment_name"`
	GitRepository      string `json:"git_repository"`
	GitBranch          string `json:"git_branch"`
	BuildPack          string `json:"build_pack"`
	PortsExposes       string `json:"ports_exposes,omitempty"`
	Name               string `json:"name,omitempty"`
	Description        string `json:"description,omitempty"`
	Domains            string `json:"domains,omitempty"`
	HealthCheckEnabled bool   `json:"health_check_enabled,omitempty"`
	HealthCheckPath    string `json:"health_check_path,omitempty"`
}

func (c *Client) CreatePublicRepo(req PublicRepoRequest) (*CreateResult, error) {
	var out CreateResult
	err := c.do(http.MethodPost, "/api/v1/applications/public", req, &out)
	return &out, err
}

type DockerfileRequest struct {
	ProjectUUID        string `json:"project_uuid"`
	ServerUUID         string `json:"server_uuid"`
	EnvironmentName    string `json:"environment_name"`
	Dockerfile         string `json:"dockerfile"`
	BuildPack          string `json:"build_pack"`
	PortsExposes       string `json:"ports_exposes,omitempty"`
	Name               string `json:"name,omitempty"`
	Description        string `json:"description,omitempty"`
	Domains            string `json:"domains,omitempty"`
	HealthCheckEnabled bool   `json:"health_check_enabled,omitempty"`
	HealthCheckPath    string `json:"health_check_path,omitempty"`
}

func (c *Client) CreateDockerfile(req DockerfileRequest) (*CreateResult, error) {
	req.Dockerfile = base64.StdEncoding.EncodeToString([]byte(req.Dockerfile))
	var out CreateResult
	err := c.do(http.MethodPost, "/api/v1/applications/dockerfile", req, &out)
	return &out, err
}

func (c *Client) UpdateApplicationDomains(uuid, domains string) error {
	return c.do(http.MethodPatch, "/api/v1/applications/"+uuid, map[string]string{"domains": domains}, nil)
}

// UpdateApplicationPortsExposes overrides the exposed port after creation.
// Needed for dockerfile-type apps: Coolify's create endpoint is meant to
// infer the port from the Dockerfile's EXPOSE line, but as of Coolify 4.1.2
// it reads the still-base64-encoded request field instead of the decoded
// one, so the parse never matches and it silently falls back to 80
// (app/Http/Controllers/Api/ApplicationsController.php:1717). This patches
// the real port in before the app is deployed.
func (c *Client) UpdateApplicationPortsExposes(uuid, port string) error {
	return c.do(http.MethodPatch, "/api/v1/applications/"+uuid, map[string]string{"ports_exposes": port}, nil)
}

func (c *Client) DeployApplication(uuid string) error {
	return c.do(http.MethodPost, "/api/v1/applications/"+uuid+"/start", nil, nil)
}

func (c *Client) StopApplication(uuid string) error {
	return c.do(http.MethodPost, "/api/v1/applications/"+uuid+"/stop", nil, nil)
}

func (c *Client) StartApplication(uuid string) error {
	return c.do(http.MethodGet, "/api/v1/applications/"+uuid+"/start", nil, nil)
}

func (c *Client) DeleteApplication(uuid string) error {
	return c.do(http.MethodDelete, "/api/v1/applications/"+uuid, nil, nil)
}

type Application struct {
	UUID          string `json:"uuid"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Status        string `json:"status"`
	FQDN          string `json:"fqdn"`
	GitRepository string `json:"git_repository"`
}

func (c *Client) ListApplications() ([]Application, error) {
	var out []Application
	err := c.do(http.MethodGet, "/api/v1/applications", nil, &out)
	return out, err
}

// --- Services (raw docker-compose) ---

type ServiceRequest struct {
	ProjectUUID      string `json:"project_uuid"`
	ServerUUID       string `json:"server_uuid"`
	EnvironmentName  string `json:"environment_name"`
	Name             string `json:"name,omitempty"`
	Description      string `json:"description,omitempty"`
	DockerComposeRaw string `json:"docker_compose_raw"`
	InstantDeploy    bool   `json:"instant_deploy"`
}

type ServiceCreateResult struct {
	UUID    string   `json:"uuid"`
	Domains []string `json:"domains"`
}

func (c *Client) CreateService(req ServiceRequest) (*ServiceCreateResult, error) {
	req.DockerComposeRaw = base64.StdEncoding.EncodeToString([]byte(req.DockerComposeRaw))
	var out ServiceCreateResult
	err := c.do(http.MethodPost, "/api/v1/services", req, &out)
	return &out, err
}

func (c *Client) StopService(uuid string) error {
	return c.do(http.MethodPost, "/api/v1/services/"+uuid+"/stop", nil, nil)
}

func (c *Client) StartService(uuid string) error {
	return c.do(http.MethodGet, "/api/v1/services/"+uuid+"/start", nil, nil)
}

func (c *Client) DeleteService(uuid string) error {
	return c.do(http.MethodDelete, "/api/v1/services/"+uuid, nil, nil)
}

type Service struct {
	UUID        string `json:"uuid"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
}

func (c *Client) ListServices() ([]Service, error) {
	var out []Service
	err := c.do(http.MethodGet, "/api/v1/services", nil, &out)
	return out, err
}

// --- Environments (for building dashboard links, which are keyed by
// environment uuid rather than the environment_name used elsewhere) ---

type Environment struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

func (c *Client) ListEnvironments(projectUUID string) ([]Environment, error) {
	var out []Environment
	err := c.do(http.MethodGet, "/api/v1/projects/"+projectUUID+"/environments", nil, &out)
	return out, err
}
