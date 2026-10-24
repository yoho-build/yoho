package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/yoho-build/yoho/internal/selfupdate"
)

func TestUpgradeAliasesAndJSON(t *testing.T) {
	for _, name := range []string{"upgrade", "update"} {
		t.Run(name, func(t *testing.T) {
			g := &globals{json: true, configPath: "/nonexistent/yoho.yml"}
			var output bytes.Buffer
			root := &cobra.Command{Use: "yoho"}
			root.SetOut(&output)
			root.AddCommand(newUpgradeCmd(g, func(ctx context.Context, current string) (selfupdate.Result, error) {
				if current != Version || ctx == nil {
					t.Fatal("missing version or context")
				}
				return selfupdate.Result{Previous: current, Version: "v0.5.2", Path: "/tmp/yoho", Updated: true}, nil
			}))
			root.SetArgs([]string{name})
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			var event map[string]any
			decoder := json.NewDecoder(&output)
			for {
				var next map[string]any
				err := decoder.Decode(&next)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				event = next
			}
			if event["event"] != "upgrade" || event["version"] != "v0.5.2" || event["updated"] != true {
				t.Fatalf("event=%v", event)
			}
		})
	}
}

func TestUpgradeFailureAndArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"unexpected"}} {
		var output bytes.Buffer
		calls := 0
		cmd := newUpgradeCmd(&globals{json: true}, func(context.Context, string) (selfupdate.Result, error) {
			calls++
			return selfupdate.Result{}, errors.New("verification failed")
		})
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		cmd.SetOut(&output)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatal("wanted error")
		}
		if len(args) > 0 && calls != 0 {
			t.Fatal("arguments invoked updater")
		}
		if len(args) == 0 && !strings.Contains(output.String(), `"event":"error"`) {
			t.Fatalf("missing structured error: %s", output.String())
		}
	}
}
