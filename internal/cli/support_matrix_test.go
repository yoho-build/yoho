package cli

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/config"
)

// TestSupportMatrixCLIRefusals asserts that every CLI-enforced "No" cell in
// docs/site/support-matrix.md fails with the documented error string.
func TestSupportMatrixCLIRefusals(t *testing.T) {
	t.Setenv("YOHO_ACCESS", "")
	t.Setenv("YOHO_DESTINATION", "")

	writeApp := func(t *testing.T, yohoYAML string) string {
		t.Helper()
		dir := t.TempDir()
		p := filepath.Join(dir, "yoho.yml")
		if err := os.WriteFile(p, []byte(yohoYAML), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  web:\n    image: nginx:alpine\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	swarmYAML := `
app: shop
servers:
  m1: {ssh: yoho@203.0.113.10}
destinations:
  production: {servers: [m1], runtime: swarm}
`
	scopedYAML := `
app: shop
servers:
  primary: {ssh: yoho@203.0.113.10}
destinations:
  production: {servers: [primary], access: scoped}
`
	previewScopedYAML := `
app: shop
servers:
  primary: {ssh: yoho@203.0.113.10}
destinations:
  staging: {servers: [primary]}
previews:
  from: staging
  host: "{name}.preview.example.com"
`

	t.Run("secrets rotate --generated on swarm", func(t *testing.T) {
		const want = "--generated supports the compose runtime only"
		_, err := checkGeneratedFlags([]string{"POSTGRES_PASSWORD"}, nil, "swarm")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("got %v, want %s", err, want)
		}
	})

	cases := []struct {
		name string
		yaml string
		env  map[string]string
		args []string
		want string
	}{
		{
			name: "backup drill on swarm",
			yaml: swarmYAML,
			args: []string{"backup", "drill"},
			want: "backup drill supports compose Destinations only",
		},
		{
			name: "app run on swarm",
			yaml: swarmYAML,
			args: []string{"app", "run", "web", "--", "true"},
			want: "app run is not available on runtime swarm",
		},
		{
			name: "backup run with scoped access",
			yaml: scopedYAML,
			args: []string{"backup", "run"},
			want: "`yoho backup run` is not available with scoped access to shop/production",
		},
		{
			name: "backup drill with scoped access",
			yaml: scopedYAML,
			args: []string{"backup", "drill"},
			want: "`yoho backup drill` is not available with scoped access to shop/production",
		},
		{
			name: "secrets rotate with scoped access",
			yaml: scopedYAML,
			args: []string{"secrets", "rotate"},
			want: "`yoho secrets rotate` is not available with scoped access to shop/production",
		},
		{
			name: "schedule install with scoped access",
			yaml: scopedYAML,
			args: []string{"schedule", "install"},
			want: "`yoho schedule install` is not available with scoped access to shop/production",
		},
		{
			name: "app run with scoped access",
			yaml: scopedYAML,
			args: []string{"app", "run", "web", "--", "true"},
			want: "`yoho app run` is not available with scoped access to shop/production",
		},
		{
			name: "preview up with YOHO_ACCESS=scoped",
			yaml: previewScopedYAML,
			env:  map[string]string{"YOHO_ACCESS": "scoped"},
			args: []string{"preview", "up", "--name", "pr-1"},
			want: "preview commands require operator access",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			path := writeApp(t, tc.yaml)
			g := &globals{configPath: path}
			root := &cobra.Command{Use: "yoho", SilenceUsage: true, SilenceErrors: true}
			for _, add := range []func(*globals) *cobra.Command{appCmds, backupCmd, secretsCmd, scheduleCmd, previewCmd} {
				root.AddCommand(add(g))
			}
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(tc.args)
			err := root.ExecuteContext(context.Background())
			if err == nil {
				t.Fatalf("expected error containing %q, got nil (out=%q)", tc.want, out.String())
			}
			combined := out.String() + "\n" + err.Error()
			if !strings.Contains(combined, tc.want) {
				t.Errorf("got output+err:\n%s\nwant substring %q", combined, tc.want)
			}
		})
	}
}

// TestSupportMatrixDocumentsRefusals checks the actual expectations in all
// three refusal suites, so changing an expectation requires updating the page.
func TestSupportMatrixDocumentsRefusals(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("..", "..", "docs", "site", "support-matrix.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range []string{"cli", "config", "composefile"} {
		path := filepath.Join("..", pkg, "support_matrix_test.go")
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		ast.Inspect(file, func(n ast.Node) bool {
			var value ast.Expr
			switch n := n.(type) {
			case *ast.KeyValueExpr:
				if key, ok := n.Key.(*ast.Ident); ok && key.Name == "want" {
					value = n.Value
				}
			case *ast.ValueSpec:
				if len(n.Names) == 1 && n.Names[0].Name == "want" && len(n.Values) == 1 {
					value = n.Values[0]
				}
			}
			if value == nil {
				return true
			}
			count++
			var want string
			switch value := value.(type) {
			case *ast.BasicLit:
				var err error
				want, err = strconv.Unquote(value.Value)
				if err != nil {
					t.Fatal(err)
				}
			case *ast.Ident:
				if pkg != "config" || value.Name != "RolesMessage" {
					t.Fatalf("unsupported want at %s", fset.Position(value.Pos()))
				}
				want = config.RolesMessage
			default:
				t.Fatalf("unsupported want at %s", fset.Position(value.Pos()))
			}
			if !strings.Contains(string(page), want) {
				t.Errorf("support-matrix.md is missing %q (%s)", want, fset.Position(value.Pos()))
			}
			return true
		})
		if count == 0 {
			t.Errorf("no refusal expectations found in %s", path)
		}
	}
}
