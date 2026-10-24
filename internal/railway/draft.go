// Package railway drafts a Compose file and a Yoho file from a names-only
// description of a Railway project, or from one service's railway.json /
// railway.toml. It never talks to Railway and never accepts secret values.
package railway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// InputError is a problem in the file the operator passed. Hint is the next
// step. Error text never includes a secret value.
type InputError struct {
	Message string
	Hint    string
}

func (e *InputError) Error() string { return e.Message }

// Request is one draft. Service and Project apply to a one-service
// railway.json or railway.toml. Project overrides an inventory's project.
type Request struct {
	Filename string
	Data     []byte
	Service  string
	Project  string
}

// Result is the text to review. Compose and Yoho end in a newline.
// SecretLines are `.yoho/secrets` references (no values). SecretKeys are
// those names. Generated keys are created on the Server and are not listed
// in SecretLines.
type Result struct {
	Compose     string
	Yoho        string
	SecretLines []string
	SecretKeys  []string
	Generated   []string
	Warnings    []string
	Services    []string
	Volumes     []string
}

const maxBytes = 1 << 20

var (
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	keyPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	hostPattern = regexp.MustCompile(`^(\*\.)?(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
	pathPattern = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
	// urlPassword matches a URL userinfo password (scheme://user:secret@host).
	urlPassword = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^/\s:@]*:[^/\s@]+@`)
)

// Draft reads data and returns a Compose file and a Yoho file. filename
// selects JSON or TOML (.toml). Anything else is JSON.
func Draft(r Request) (*Result, error) {
	if len(r.Data) == 0 {
		return nil, &InputError{Message: "input is empty", Hint: "pass a names-only inventory JSON, or a railway.json / railway.toml"}
	}
	if len(r.Data) > maxBytes {
		return nil, &InputError{Message: "input is larger than 1 MiB", Hint: "pass the inventory or one config-as-code file, not a database dump"}
	}
	data := bytes.TrimPrefix(r.Data, []byte{0xEF, 0xBB, 0xBF})
	if strings.EqualFold(path.Ext(r.Filename), ".toml") {
		return draftRailway(r, data, true)
	}
	var probe map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&probe); err != nil {
		// A railway.json has many schema fields. Probe with unknown fields
		// allowed, then decide from the keys.
		probe = nil
		if err2 := json.Unmarshal(data, &probe); err2 != nil {
			return nil, &InputError{Message: "input is not JSON", Hint: "inventory files are JSON; a Railway config-as-code file may be railway.json or railway.toml"}
		}
	}
	if _, ok := probe["services"]; ok {
		return draftInventory(r, data)
	}
	if _, ok := probe["build"]; ok {
		return draftRailway(r, data, false)
	}
	if _, ok := probe["deploy"]; ok {
		return draftRailway(r, data, false)
	}
	if _, ok := probe["$schema"]; ok {
		return draftRailway(r, data, false)
	}
	return nil, &InputError{
		Message: "unrecognized file (no services, build, or deploy)",
		Hint:    "use a names-only inventory ({\"project\",\"services\"}) or a railway.json / railway.toml. Yoho does not read .railway/railway.ts",
	}
}

type inventory struct {
	Project   string    `json:"project"`
	Hostnames []string  `json:"hostnames,omitempty"`
	Services  []service `json:"services"`
}

type service struct {
	Name             string            `json:"name"`
	Image            string            `json:"image,omitempty"`
	Dockerfile       string            `json:"dockerfile,omitempty"`
	Command          []string          `json:"command,omitempty"`
	StartCommand     string            `json:"start_command,omitempty"`
	ReleaseCommand   []string          `json:"release_command,omitempty"`
	PreDeployCommand string            `json:"pre_deploy_command,omitempty"`
	HealthcheckPath  string            `json:"healthcheck_path,omitempty"`
	Healthcheck      []string          `json:"healthcheck,omitempty"`
	Port             int               `json:"port,omitempty"`
	Hosts            []string          `json:"hosts,omitempty"`
	Public           bool              `json:"public,omitempty"`
	Restart          string            `json:"restart,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	SecretKeys       []string          `json:"secret_keys,omitempty"`
	VolumeMount      string            `json:"volume_mount,omitempty"`
	Volume           string            `json:"volume,omitempty"`
	Cron             string            `json:"cron,omitempty"`
	DependsOn        []string          `json:"depends_on,omitempty"`
}

func draftInventory(r Request, data []byte) (*Result, error) {
	if err := rejectSecretValues(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var inv inventory
	if err := dec.Decode(&inv); err != nil {
		return nil, &InputError{Message: "inventory JSON: " + err.Error(), Hint: "the field list is in docs/site/migrating-from-railway.md; values belong in 1Password or Bitwarden, not in this file"}
	}
	if dec.More() {
		return nil, &InputError{Message: "inventory JSON has extra data after the object", Hint: "pass one JSON object"}
	}
	if r.Service != "" {
		return nil, &InputError{Message: "--service applies to railway.json and railway.toml", Hint: "name each service in the inventory"}
	}
	if r.Project != "" {
		inv.Project = r.Project
	}
	return render(inv, "inventory "+path.Base(blank(r.Filename, "inventory.json")), nil)
}

type railwayFile struct {
	Build        railwayBuild   `json:"build"`
	Deploy       railwayDeploy  `json:"deploy"`
	Environments map[string]any `json:"environments"`
}

type railwayBuild struct {
	Builder        string `json:"builder"`
	DockerfilePath string `json:"dockerfilePath"`
}

type railwayDeploy struct {
	StartCommand            string `json:"startCommand"`
	PreDeployCommand        any    `json:"preDeployCommand"`
	HealthcheckPath         string `json:"healthcheckPath"`
	HealthcheckTimeout      any    `json:"healthcheckTimeout"`
	RestartPolicyType       string `json:"restartPolicyType"`
	RestartPolicyMaxRetries any    `json:"restartPolicyMaxRetries"`
	CronSchedule            string `json:"cronSchedule"`
	NumReplicas             any    `json:"numReplicas"`
	RegistryCredentials     any    `json:"registryCredentials"`
	SleepApplication        any    `json:"sleepApplication"`
}

func draftRailway(r Request, data []byte, tomlFile bool) (*Result, error) {
	var raw any
	if tomlFile {
		if err := toml.Unmarshal(data, &raw); err != nil {
			return nil, &InputError{Message: "railway.toml: " + err.Error(), Hint: "this draft reads one service's build and deploy tables, not .railway/railway.ts"}
		}
	} else if err := json.Unmarshal(data, &raw); err != nil {
		return nil, &InputError{Message: "railway.json: " + err.Error(), Hint: "this draft reads one service's build and deploy object"}
	}
	if m, ok := raw.(map[string]any); ok {
		if _, ok := m["services"]; ok {
			return nil, &InputError{Message: "a TOML file with services is not an inventory", Hint: "inventory files are JSON. railway.toml is one service's build and deploy tables"}
		}
	}
	if err := walkSecrets(raw, ""); err != nil {
		return nil, err
	}
	var file railwayFile
	var err error
	if tomlFile {
		err = toml.Unmarshal(data, &file)
	} else {
		err = json.Unmarshal(data, &file)
	}
	if err != nil {
		return nil, &InputError{Message: "config-as-code: " + err.Error(), Hint: "check the file against https://railway.com/railway.schema.json"}
	}
	if file.Deploy.RegistryCredentials != nil {
		return nil, &InputError{Message: "config-as-code contains registryCredentials", Hint: "remove registryCredentials before drafting; a registry password does not belong in the file Yoho reads"}
	}
	svcName := r.Service
	if svcName == "" {
		svcName = "web"
	}
	project := r.Project
	if project == "" {
		project = "app"
	}
	pre, err := oneCommand(file.Deploy.PreDeployCommand)
	if err != nil {
		return nil, err
	}
	svc := service{
		Name:             svcName,
		Dockerfile:       file.Build.DockerfilePath,
		StartCommand:     file.Deploy.StartCommand,
		PreDeployCommand: pre,
		HealthcheckPath:  file.Deploy.HealthcheckPath,
		Restart:          file.Deploy.RestartPolicyType,
		Cron:             file.Deploy.CronSchedule,
	}
	if svc.Dockerfile != "" || strings.EqualFold(file.Build.Builder, "DOCKERFILE") {
		if svc.Dockerfile == "" {
			svc.Dockerfile = "Dockerfile"
		}
	}
	var extra []string
	if file.Build.Builder != "" && !strings.EqualFold(file.Build.Builder, "DOCKERFILE") && file.Build.DockerfilePath == "" {
		extra = append(extra, svc.Name+": Railway builder "+file.Build.Builder+" is not converted; set image or a Dockerfile before deploying")
	}
	if file.Deploy.HealthcheckTimeout != nil {
		extra = append(extra, "Railway healthcheckTimeout is not copied; the compose healthcheck uses a 5s interval")
	}
	if file.Deploy.NumReplicas != nil && fmt.Sprint(file.Deploy.NumReplicas) != "1" {
		extra = append(extra, "Railway numReplicas is not copied; a Destination runs the compose project on its Servers")
	}
	if truthy(file.Deploy.SleepApplication) {
		extra = append(extra, svc.Name+": Railway sleepApplication is not copied; the Service stays running")
	}
	if len(file.Environments) > 0 {
		extra = append(extra, "Railway environments overrides are not copied; the top-level build and deploy are used")
	}
	if file.Deploy.RestartPolicyMaxRetries != nil {
		extra = append(extra, "restartPolicyMaxRetries is not copied; ON_FAILURE becomes restart: on-failure")
	}
	inv := inventory{Project: project, Services: []service{svc}}
	return render(inv, "config-as-code "+path.Base(blank(r.Filename, "railway.json")), extra)
}

func oneCommand(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "", nil
	case string:
		return t, nil
	case []any:
		if len(t) != 1 {
			return "", &InputError{Message: "preDeployCommand must be one string", Hint: "the Railway schema allows a string or an array of one string; put argv in the inventory release_command field"}
		}
		s, ok := t[0].(string)
		if !ok || s == "" {
			return "", &InputError{Message: "preDeployCommand must be one string", Hint: "the Railway schema allows a string or an array of one string"}
		}
		return s, nil
	case []string:
		if len(t) != 1 || t[0] == "" {
			return "", &InputError{Message: "preDeployCommand must be one string", Hint: "the Railway schema allows a string or an array of one string; put argv in the inventory release_command field"}
		}
		return t[0], nil
	default:
		return "", &InputError{Message: "preDeployCommand must be a string", Hint: "the Railway schema allows a string or an array of one string"}
	}
}

func truthy(v any) bool {
	b, ok := v.(bool)
	return ok && b
}

func blank(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func rejectSecretValues(data []byte) error {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return &InputError{Message: "inventory JSON: " + err.Error(), Hint: "the field list is in docs/site/migrating-from-railway.md"}
	}
	return walkSecrets(raw, "")
}

func walkSecrets(v any, at string) error {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			lk := strings.ToLower(k)
			here := k
			if at != "" {
				here = at + "." + k
			}
			if lk == "registrycredentials" && child != nil {
				return &InputError{Message: here + " is set", Hint: "remove registryCredentials; it can hold a password"}
			}
			if s, ok := child.(string); ok && s != "" && sensitiveKey(lk) {
				return &InputError{Message: here + " holds a value", Hint: "keep the name in secret_keys and put the value in 1Password or Bitwarden"}
			}
			if err := walkSecrets(child, here); err != nil {
				return err
			}
		}
	case []any:
		for i, child := range t {
			if err := walkSecrets(child, fmt.Sprintf("%s[%d]", at, i)); err != nil {
				return err
			}
		}
	case string:
		if strings.Contains(t, "${{") {
			return &InputError{Message: labelAt(at) + " contains an unresolved Railway ${{ }} reference", Hint: "put the variable name in secret_keys; this draft does not call Railway"}
		}
		if urlPassword.MatchString(t) {
			return &InputError{Message: labelAt(at) + " looks like a URL with a password", Hint: "remove the URL. Point the app at the db Service with a secret reference, not at Railway's DATABASE_URL"}
		}
	}
	return nil
}

func labelAt(at string) string {
	if at == "" {
		return "a string"
	}
	return at
}

func sensitiveKey(lk string) bool {
	switch lk {
	case "password", "secret", "token", "api_key", "apikey", "private_key", "database_url", "dsn", "credentials":
		return true
	}
	return strings.HasSuffix(lk, "_password") || strings.HasSuffix(lk, "_secret") || strings.HasSuffix(lk, "_token") || strings.HasSuffix(lk, "_api_key")
}
