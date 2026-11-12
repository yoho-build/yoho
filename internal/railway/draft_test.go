package railway

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/secrets"
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

func TestReferencesInventoryGolden(t *testing.T) {
	dir := filepath.Join("..", "..", "examples", "quickstart", "railway")
	in := filepath.Join(dir, "inventory-references.json")
	data, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	d, err := Draft(Request{Filename: in, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if contains(d.Generated, "POSTGRES_PASSWORD") {
		t.Fatal("composed password must be resolved locally")
	}
	password := "POSTGRES_PASSWORD=$(op read op://Production/shop/POSTGRES_PASSWORD)"
	url := "DATABASE_URL=postgres://postgres:${POSTGRES_PASSWORD}@db:5432/postgres"
	lines := strings.Join(d.SecretLines, "\n")
	if !strings.HasPrefix(lines, password+"\n") || !strings.Contains(lines, url) || strings.Count(lines, "DATABASE_URL=") != 1 {
		t.Fatalf("secret dependencies: %s", lines)
	}
	if !strings.Contains(d.Compose, "DB_HOST: db") || strings.Contains(d.Compose, "DATABASE_URL:") || !strings.Contains(d.Compose, "- DATABASE_URL") || strings.Contains(d.Compose, "generate:") {
		t.Fatalf("compose: %s", d.Compose)
	}
	assertGolden(t, filepath.Join(dir, "compose.references.draft.yaml"), d.Compose)
	assertGolden(t, filepath.Join(dir, "yoho.references.draft.yml"), d.Yoho)

	// Load uses the real dotenv parser; no provider or shell command is needed.
	secretDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(secretDir, ".yoho"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretDir, ".yoho", "secrets"), []byte("POSTGRES_PASSWORD=pw\n"+url+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := secrets.Load(context.Background(), secretDir, "", config.SecretsConfig{}, secrets.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := store.Get("DATABASE_URL"); !ok || value != "postgres://postgres:pw@db:5432/postgres" {
		t.Fatal("composed DATABASE_URL did not expand correctly")
	}
}

func TestInventoryReferences(t *testing.T) {
	cases := []struct {
		name, key, value, want, hint string
	}{
		{"domain and port", "URL", "http://${{api.RAILWAY_PRIVATE_DOMAIN}}:${{api.PORT}}/v1", "http://api:3000/v1", ""},
		{"display name", "HOST", "${{ My API.RAILWAY_PRIVATE_DOMAIN }}", "my-api", ""},
		{"literal", "MODE", "${{api.MODE}}", "production", ""},
		{"postgres host", "HOST", "${{POSTGRESQL.PGHOST}}", "db", ""},
		{"postgres port", "DB_PORT", "${{db.PORT}}", "5432", ""},
		{"postgres user default", "USER", "${{db.PGUSER}}", "postgres", ""},
		{"postgres database default", "DB", "${{db.PGDATABASE}}", "postgres", ""},
		{"shared", "X", "${{shared.X}}", "", "X=$(op read op://Production/shop/X)"},
		{"unqualified", "X", "${{X}}", "", "add X to secret_keys"},
		{"public domain", "X", "${{web.RAILWAY_PUBLIC_DOMAIN}}", "", "add X to secret_keys"},
		{"tcp proxy", "X", "${{api.RAILWAY_TCP_PROXY_PORT}}", "", "add X to secret_keys"},
		{"unknown service", "X", "${{absent.PORT}}", "", "add X to secret_keys"},
		{"missing port", "X", "${{web.PORT}}", "", "add X to secret_keys"},
		{"unknown variable", "X", "${{db.UNKNOWN}}", "", "add X to secret_keys"},
		{"referenced target", "X", "${{api.INDIRECT}}", "", "add X to secret_keys"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := inventory{Project: "shop", Services: []service{
				{Name: "api", Image: "app:1", Port: 3000, Env: map[string]string{"MODE": "production", "INDIRECT": "${{db.PGHOST}}"}},
				{Name: "web", Image: "app:1", Env: map[string]string{tc.key: tc.value}},
				{Name: "my-api", Image: "app:1"},
				{Name: "db", Image: "postgres:17"},
			}}
			data, err := json.Marshal(inv)
			if err != nil {
				t.Fatal(err)
			}
			d, err := Draft(Request{Filename: "inventory.json", Data: data})
			if tc.hint != "" {
				var input *InputError
				if !errors.As(err, &input) || !strings.Contains(input.Hint, tc.hint) || !strings.Contains(input.Message, "cannot be resolved offline") {
					t.Fatalf("got %#v, want hint %q", err, tc.hint)
				}
				if parts := referencePattern.FindStringSubmatch(tc.value); len(parts) > 0 && !strings.Contains(input.Message, "${{"+strings.TrimSpace(parts[1])+"."+parts[2]+"}}") {
					t.Fatalf("missing exact reference: %s", input.Message)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(d.Compose, tc.want) {
				t.Fatalf("missing %q in %s", tc.want, d.Compose)
			}
		})
	}
}

func TestReferencesSecretGuard(t *testing.T) {
	cases := []struct{ file, body string }{
		{"railway.json", `{"deploy":{"startCommand":"${{db.PGHOST}}"}}`},
		{"railway.toml", "[deploy]\nstartCommand = '${{db.PGHOST}}'"},
		{"inventory.json", `{"project":"shop","services":[{"name":"web","image":"app:1","start_command":"${{db.PGHOST}}"}]}`},
		{"inventory.json", `{"project":"shop","services":[{"name":"web","image":"app:1","secret_keys":["${{db.PGHOST}}"]}]}`},
		{"inventory.json", `{"project":"shop","services":[{"name":"web","image":"app:1","env":{"DATABASE_URL":"postgres://u:pw@h/db"}}]}`},
		{"inventory.json", `{"project":"shop","services":[{"name":"web","image":"app:1","env":{"DATABASE_URL":"postgres://u:pw@${{db.PGHOST}}/db"}}]}`},
		{"inventory.json", `{"project":"shop","services":[{"name":"web","image":"app:1","env":{"URL":"postgres://u:pw${{db.PGPASSWORD}}@h/db"}}]}`},
	}
	for _, tc := range cases {
		_, err := Draft(Request{Filename: tc.file, Data: []byte(tc.body)})
		if err == nil || strings.Contains(err.Error(), "pw") {
			t.Fatalf("expected value-free error, got %v", err)
		}
	}
}

func TestComposedReferences(t *testing.T) {
	for _, variable := range []string{"DATABASE_URL", "DATABASE_PRIVATE_URL", "PGPASSWORD", "POSTGRES_PASSWORD"} {
		t.Run(variable, func(t *testing.T) {
			inv := inventory{Project: "shop", Services: []service{
				{Name: "web", Image: "app:1", SecretKeys: []string{"CONNECTION"}, Env: map[string]string{"CONNECTION": "prefix-${{db." + variable + "}}-suffix"}},
				{Name: "db", Image: "postgres:17", Env: map[string]string{"POSTGRES_USER": "appuser", "POSTGRES_DB": "appdb"}},
			}}
			d, err := render(inv, "inventory test", nil)
			if err != nil {
				t.Fatal(err)
			}
			want := "${POSTGRES_PASSWORD}"
			if strings.Contains(variable, "URL") {
				want = "postgres://appuser:${POSTGRES_PASSWORD}@db:5432/appdb"
			}
			if !contains(d.SecretLines, "CONNECTION=prefix-"+want+"-suffix") || len(d.Generated) != 0 || strings.Count(d.Compose, "- CONNECTION") != 1 {
				t.Fatalf("incorrect composed reference: %+v", d)
			}
		})
	}
}

func TestReferenceConflictsAndAmbiguity(t *testing.T) {
	for _, different := range []bool{false, true} {
		inv := inventory{Project: "shop", Services: []service{
			{Name: "a", Image: "app:1", Env: map[string]string{"URL": "${{db.DATABASE_URL}}"}},
			{Name: "b", Image: "app:1", Env: map[string]string{"URL": "${{db.DATABASE_PRIVATE_URL}}"}},
			{Name: "db", Image: "postgres:17"},
		}}
		if different {
			inv.Services[1].Env["URL"] += "/different"
		}
		_, err := render(inv, "inventory test", nil)
		if different {
			if err == nil || !strings.Contains(err.Error(), "env.URL resolves differently in a and b; rename one") {
				t.Fatal(err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
	inv := inventory{Services: []service{{Name: "db", Image: "postgres:17"}, {Name: "db2", Image: "postgres:17"}}}
	if resolveTarget(&inv, "Postgres") != nil || resolveTarget(&inv, "DB") == nil {
		t.Fatal("ambiguous template alias must fail; exact name must still match")
	}
}

func TestComposedReferenceLiterals(t *testing.T) {
	for _, value := range []string{
		"postgres://${{db.PGUSER}}:${{db.PGPASSWORD}}@${{db.PGHOST}}:${{db.PGPORT}}/${{db.PGDATABASE}}",
		`$(must-not-run) "quoted" # literal ${OTHER} \\ ${{db.PGPASSWORD}}`,
		`${{api.LITERAL}}-${{db.PGPASSWORD}}`,
	} {
		inv := inventory{Project: "shop", Services: []service{
			{Name: "web", Image: "app:1", Env: map[string]string{"CONNECTION": value}},
			{Name: "api", Image: "app:1", Env: map[string]string{"LITERAL": "$(must-not-run)"}},
			{Name: "db", Image: "postgres:17"},
		}}
		data, err := json.Marshal(inv)
		if err != nil {
			t.Fatal(err)
		}
		d, err := Draft(Request{Filename: "inventory.json", Data: data})
		if err != nil {
			t.Fatal(err)
		}
		var line string
		for _, candidate := range d.SecretLines {
			if strings.HasPrefix(candidate, "CONNECTION=") {
				line = candidate
			}
		}
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, ".yoho"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".yoho", "secrets"), []byte("POSTGRES_PASSWORD=pw\n"+line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		store, err := secrets.Load(context.Background(), dir, "", config.SecretsConfig{}, secrets.LoadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		want := strings.NewReplacer(
			"${{db.PGUSER}}", "postgres", "${{db.PGPASSWORD}}", "pw",
			"${{db.PGHOST}}", "db", "${{db.PGPORT}}", "5432",
			"${{db.PGDATABASE}}", "postgres", "${{api.LITERAL}}", "$(must-not-run)",
		).Replace(value)
		if got, ok := store.Get("CONNECTION"); !ok || got != want {
			t.Fatal("dotenv did not preserve composed literal text")
		}
	}
}

func TestInventoryEnvWithoutReferences(t *testing.T) {
	body := `{"project":"shop","services":[{"name":"web","image":"app:1","env":{"VAPID_SUBJECT":"mailto:ops@shop.example.com","IMG":"ghcr.io/o/r:1.2@sha256:abc"}}]}`
	d, err := Draft(Request{Filename: "inventory.json", Data: []byte(body)})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"mailto:ops@shop.example.com", "ghcr.io/o/r:1.2@sha256:abc"} {
		if !strings.Contains(d.Compose, value) {
			t.Fatalf("missing literal %q", value)
		}
	}
}

func TestReferenceValidationOrder(t *testing.T) {
	for _, tc := range []struct {
		name, project, db, key, value, wantError, wantEnv string
	}{
		{"database identifier", "shop", "shop-prod", "DATABASE_URL", "${{db.DATABASE_URL}}", "POSTGRES_DB must be letters, digits, and underscores", ""},
		{"project", "", "postgres", "DATABASE_URL", "${{missing.DATABASE_URL}}", "project", ""},
		{"dropped variable", "shop", "postgres", "RAILWAY_TOKEN", "${{db.DATABASE_URL}}", "", ""},
		{"dropped unresolved variable", "shop", "postgres", "RAILWAY_TOKEN", "${{missing.X}}", "", ""},
		{"sensitive literal reference", "shop", "postgres", "DSN", "host=${{db.PGHOST}} port=${{db.PGPORT}} user=${{db.PGUSER}} dbname=${{db.PGDATABASE}}", "", "host=db port=5432 user=postgres dbname=postgres"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := inventory{Project: tc.project, Services: []service{
				{Name: "web", Image: "app:1", Env: map[string]string{tc.key: tc.value}},
				{Name: "db", Image: "postgres:17", Env: map[string]string{"POSTGRES_DB": tc.db}},
			}}
			data, err := json.Marshal(inv)
			if err != nil {
				t.Fatal(err)
			}
			d, err := Draft(Request{Filename: "inventory.json", Data: data})
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) || strings.Contains(err.Error(), "cannot be resolved offline") {
					t.Fatalf("got %v, want validation error containing %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantEnv != "" {
				if !strings.Contains(d.Compose, "DSN: "+tc.wantEnv) || contains(d.SecretKeys, "DSN") {
					t.Fatal("non-secret reference must remain a Compose environment literal")
				}
			} else if !contains(d.Generated, "POSTGRES_PASSWORD") || contains(d.SecretKeys, "RAILWAY_TOKEN") || strings.Contains(d.Compose, "- RAILWAY_TOKEN") || strings.Contains(strings.Join(d.Warnings, "\n"), "is composed") {
				t.Fatal("dropped Railway variable changed secret state")
			}
		})
	}
}

func TestComposedKeyConflictsWithPlainSecret(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		inv := inventory{Project: "shop", Services: []service{
			{Name: "web", Image: "app:1", SecretKeys: []string{"DATABASE_URL"}},
			{Name: "worker", Image: "app:1", Env: map[string]string{"DATABASE_URL": "${{db.DATABASE_URL}}"}},
			{Name: "db", Image: "postgres:17"},
		}}
		if reverse {
			inv.Services[0], inv.Services[1] = inv.Services[1], inv.Services[0]
		}
		_, err := render(inv, "inventory test", nil)
		var input *InputError
		if !errors.As(err, &input) || input.Message != "DATABASE_URL is composed for worker and listed in secret_keys of web; rename one" {
			t.Fatalf("expected plain-secret conflict, got %v", err)
		}
	}
	// Two consumers can both explicitly declare the same composed secret.
	inv := inventory{Project: "shop", Services: []service{
		{Name: "web", Image: "app:1", SecretKeys: []string{"DATABASE_URL"}, Env: map[string]string{"DATABASE_URL": "${{db.DATABASE_URL}}"}},
		{Name: "worker", Image: "app:1", SecretKeys: []string{"DATABASE_URL"}, Env: map[string]string{"DATABASE_URL": "${{db.DATABASE_URL}}"}},
		{Name: "db", Image: "postgres:17"},
	}}
	if _, err := render(inv, "inventory test", nil); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresPasswordReferenceKeepsGeneration(t *testing.T) {
	for _, variable := range []string{"POSTGRES_PASSWORD", "PGPASSWORD"} {
		t.Run(variable, func(t *testing.T) {
			inv := inventory{Project: "shop", Services: []service{
				{Name: "web", Image: "app:1", Env: map[string]string{"POSTGRES_PASSWORD": "${{ Postgres." + variable + " }}"}},
				{Name: "db", Image: "postgres:17"},
			}}
			d, err := render(inv, "inventory test", nil)
			if err != nil {
				t.Fatal(err)
			}
			if join(d.Generated) != "POSTGRES_PASSWORD" || len(d.SecretLines) != 0 || len(d.Warnings) != 0 || strings.Count(d.Compose, "- POSTGRES_PASSWORD") != 1 || !strings.Contains(d.Compose, "POSTGRES_PASSWORD: password32") {
				t.Fatal("direct password reference must use the generated secret without local composition")
			}
		})
	}
}

func TestMultiplePostgresReferences(t *testing.T) {
	for _, variable := range []string{"DATABASE_URL", "DATABASE_PRIVATE_URL", "PGPASSWORD", "POSTGRES_PASSWORD", "PGHOST"} {
		t.Run(variable, func(t *testing.T) {
			inv := inventory{Project: "shop", Services: []service{
				{Name: "web", Image: "app:1", Env: map[string]string{"CONNECTION": "${{db." + variable + "}}"}},
				{Name: "db", Image: "postgres:17", Volume: "db-data", SecretKeys: []string{"POSTGRES_PASSWORD"}},
				{Name: "db2", Image: "postgres:17", Volume: "db2-data", SecretKeys: []string{"POSTGRES_PASSWORD"}},
			}}
			d, err := render(inv, "inventory test", nil)
			if variable == "PGHOST" {
				if err != nil || !strings.Contains(d.Compose, "CONNECTION: db") {
					t.Fatalf("named non-secret reference should work: %v", err)
				}
				return
			}
			var input *InputError
			if !errors.As(err, &input) || !strings.Contains(input.Message, "more than one Postgres Service") || !strings.Contains(input.Hint, "add CONNECTION to secret_keys") || !strings.Contains(input.Hint, "CONNECTION=$(op read op://Production/shop/CONNECTION)") {
				t.Fatalf("expected manual composition hint, got %#v", err)
			}
		})
	}
}

func TestRotationExcludesComposedKeys(t *testing.T) {
	inv := inventory{Project: "shop", Services: []service{
		{Name: "web", Image: "app:1", SecretKeys: []string{"API_TOKEN"}, Env: map[string]string{"DATABASE_URL": "${{db.DATABASE_URL}}", "DB_PASSWORD": "${{db.PGPASSWORD}}"}},
		{Name: "db", Image: "postgres:17"},
	}}
	d, err := render(inv, "inventory test", nil)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, item := range d.Rotation {
		keys = append(keys, item.Key)
	}
	if join(keys) != "API_TOKEN,POSTGRES_PASSWORD" {
		t.Fatalf("rotation must list source secrets only: %v", keys)
	}
}

func TestComposedURLPasswordWarning(t *testing.T) {
	inv := inventory{Project: "shop", Services: []service{
		{Name: "web", Image: "app:1", Env: map[string]string{"DATABASE_URL": "${{db.DATABASE_URL}}"}},
		{Name: "db", Image: "postgres:17"},
	}}
	d, err := render(inv, "inventory test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(d.Warnings, "\n"), "POSTGRES_PASSWORD must be URL-safe (letters and digits) because it is inserted into DATABASE_URL unencoded") {
		t.Fatal("missing warning about unencoded passwords")
	}
}
