package railway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQuickstartInventoryGolden(t *testing.T) {
	in := filepath.Join("..", "..", "examples", "quickstart", "railway", "inventory.json")
	data, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	d, err := Draft(Request{Filename: in, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.Compose, "postgres://") || strings.Contains(d.Yoho, "hello-from") {
		t.Fatal("draft contains a value it should not")
	}
	if join(d.SecretKeys) != "APP_GREETING,CF_API_TOKEN,CF_TUNNEL_TOKEN" {
		t.Fatalf("secret keys %v", d.SecretKeys)
	}
	if join(d.Generated) != "POSTGRES_PASSWORD" {
		t.Fatalf("generated %v", d.Generated)
	}
	if !strings.Contains(d.Compose, "railway volumes are not") && !strings.Contains(strings.ToLower(d.Compose), "pg_restore") {
		t.Fatalf("compose header:\n%s", d.Compose)
	}
	for _, line := range d.SecretLines {
		if !strings.Contains(line, "$(op read op://Production/shop/") || strings.Contains(line, "=") && strings.Contains(line, "password32") {
			t.Fatalf("secret line %q", line)
		}
	}
	assertGolden(t, filepath.Join("..", "..", "examples", "quickstart", "railway", "compose.draft.yaml"), d.Compose)
	assertGolden(t, filepath.Join("..", "..", "examples", "quickstart", "railway", "yoho.draft.yml"), d.Yoho)
}

func TestRejectsSecretValues(t *testing.T) {
	cases := []struct {
		name string
		file string
		body string
		want string
	}{
		{"database url", "inventory.json", `{
			"project":"shop","services":[{"name":"web","image":"app:1","env":{"DATABASE_URL":"postgres://u:secret@h/db"}}]
		}`, "holds a value"},
		{"inline url", "inventory.json", `{
			"project":"shop","services":[{"name":"web","image":"app:1","start_command":"postgres://u:secret@h/db"}]
		}`, "password"},
		{"key equals value", "inventory.json", `{
			"project":"shop","services":[{"name":"web","image":"app:1","secret_keys":["APP_GREETING=hello"]}]
		}`, "KEY=value"},
		{"template", "inventory.json", `{
			"project":"shop","services":[{"name":"web","image":"app:1","env":{"HOST":"${{shared.HOST}}"}}]
		}`, "${{"},
		{"registry", "railway.json", `{
			"build":{"builder":"DOCKERFILE","dockerfilePath":"Dockerfile"},
			"deploy":{"registryCredentials":{"username":"u","password":"secret"}}
		}`, "registryCredentials"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Draft(Request{Filename: tc.file, Data: []byte(tc.body)})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v", err)
			}
			if strings.Contains(err.Error(), "secret@") || strings.Contains(err.Error(), "hello") {
				t.Fatalf("error includes a value: %v", err)
			}
		})
	}
}

func TestRailwayJSONAndTOML(t *testing.T) {
	js := []byte(`{
		"$schema": "https://railway.com/railway.schema.json",
		"build": {"builder": "DOCKERFILE", "dockerfilePath": "Dockerfile"},
		"deploy": {
			"startCommand": "/app",
			"preDeployCommand": ["/app migrate"],
			"healthcheckPath": "/up",
			"healthcheckTimeout": 30,
			"restartPolicyType": "ALWAYS",
			"numReplicas": 2
		}
	}`)
	d, err := Draft(Request{Filename: "railway.json", Data: js, Project: "shop", Service: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Compose, "build: .") || !strings.Contains(d.Compose, `restart: always`) {
		t.Fatalf("compose:\n%s", d.Compose)
	}
	if !strings.Contains(d.Compose, `["sh", "-c", "/app migrate"]`) {
		t.Fatalf("predeploy:\n%s", d.Compose)
	}
	if !strings.Contains(strings.Join(d.Warnings, "\n"), "numReplicas") || !strings.Contains(strings.Join(d.Warnings, "\n"), "healthcheckTimeout") {
		t.Fatalf("warnings %v", d.Warnings)
	}
	tomlFile := []byte(`
[build]
builder = "DOCKERFILE"
dockerfilePath = "Dockerfile"

[deploy]
startCommand = "/app worker"
restartPolicyType = "ON_FAILURE"
cronSchedule = "0 3 * * *"
`)
	d, err = Draft(Request{Filename: "railway.toml", Data: tomlFile})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Compose, "restart: on-failure") || !strings.Contains(strings.Join(d.Warnings, "\n"), "cron") {
		t.Fatalf("toml compose:\n%s\nwarnings %v", d.Compose, d.Warnings)
	}
	if !strings.Contains(d.Yoho, "app: app") || !strings.Contains(d.Compose, "web:") {
		t.Fatalf("defaults:\n%s\n%s", d.Yoho, d.Compose)
	}
}

func TestNixpacksWarnsWithoutImage(t *testing.T) {
	d, err := Draft(Request{Filename: "railway.json", Data: []byte(`{"build":{"builder":"NIXPACKS"},"deploy":{"startCommand":"npm start"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(d.Warnings, "\n"), "NIXPACKS") {
		t.Fatalf("warnings %v", d.Warnings)
	}
	if strings.Contains(d.Compose, "\n    image:") {
		t.Fatalf("should not invent an image:\n%s", d.Compose)
	}
}

func TestDropsRailwayVariables(t *testing.T) {
	body := []byte(`{"project":"shop","services":[{"name":"web","image":"app:1","env":{"RAILWAY_PUBLIC_DOMAIN":"app.up.railway.app","PORT":"8080"},"secret_keys":["RAILWAY_SERVICE_ID","API_TOKEN"]}]}`)
	d, err := Draft(Request{Filename: "inventory.json", Data: body})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.Compose, "RAILWAY_PUBLIC_DOMAIN:") || strings.Contains(d.Compose, "- RAILWAY_") {
		t.Fatalf("railway var kept:\n%s", d.Compose)
	}
	if !strings.Contains(d.Compose, "PORT:") || !strings.Contains(strings.Join(d.Warnings, "\n"), "RAILWAY_PUBLIC_DOMAIN") {
		t.Fatalf("compose:\n%s\n%v", d.Compose, d.Warnings)
	}
	if join(d.SecretKeys) != "API_TOKEN" {
		t.Fatalf("keys %v", d.SecretKeys)
	}
}

func TestServiceFlagRejectedOnInventory(t *testing.T) {
	_, err := Draft(Request{Filename: "inventory.json", Data: []byte(`{"project":"shop","services":[{"name":"web","image":"app:1"}]}`), Service: "web"})
	if err == nil || !strings.Contains(err.Error(), "--service") {
		t.Fatal(err)
	}
}

func assertGolden(t *testing.T, path, got string) {
	t.Helper()
	if os.Getenv("YOHO_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden %s (YOHO_UPDATE_GOLDEN=1): %v\n%s", path, err, got)
	}
	if string(want) != got {
		t.Fatalf("golden %s\n--- got ---\n%s", path, got)
	}
}

func join(ss []string) string { return strings.Join(ss, ",") }
