package cli

import (
	"encoding/json"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/secrets"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validSavedPlan() savedPlan {
	now := time.Now().UTC()
	return savedPlan{Format: 1, App: "shop", Destination: "production", Yoho: Version, Version: "v1", Config: "config", Secrets: "names-and-generation", Inputs: "inputs", Remote: map[string]string{"one": "release-1"}, Created: now.Add(-time.Minute), Expires: now.Add(time.Hour)}
}
func TestSavedPlanRejectsStale(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*savedPlan)
	}{
		{"expired", func(p *savedPlan) { p.Expires = time.Now().Add(-time.Second) }},
		{"Destination", func(p *savedPlan) { p.Destination = "staging" }},
		{"App", func(p *savedPlan) { p.App = "other" }},
		{"Yoho", func(p *savedPlan) { p.Yoho = "old" }},
		{"config", func(p *savedPlan) { p.Config = "new-config" }},
		{"remote Release", func(p *savedPlan) { p.Remote = map[string]string{"one": "release-2"} }},
		{"secret generation", func(p *savedPlan) { p.Secrets = "new-generation" }},
		{"build input", func(p *savedPlan) { p.Inputs = "new-inputs" }},
		{"git revision", func(p *savedPlan) { p.SourceRevision = "new-head" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			approved := validSavedPlan()
			current := approved
			tt.change(&current)
			err := current.validate("shop", "production", Version, time.Now())
			if err == nil {
				err = approved.compare(&current)
			}
			if err == nil || mutationCode(err, false) != 15 {
				t.Fatalf("want code 15: %v", err)
			}
		})
	}
}
func TestSavedPlanRoundTripAndStrictDecode(t *testing.T) {
	p := validSavedPlan()
	file := filepath.Join(t.TempDir(), "plan.json")
	b, _ := json.Marshal(p)
	if err := os.WriteFile(file, b, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readSavedPlan(file)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.compare(got); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{string(b) + " {}", `{"format_version":1,"unexpected":true}`, `not-json`} {
		os.WriteFile(file, []byte(bad), 0600)
		if _, err = readSavedPlan(file); mutationCode(err, false) != 15 {
			t.Fatalf("invalid artifact accepted: %v", err)
		}
	}
}
func TestSavedPlanDiffNeverPrintsInputs(t *testing.T) {
	p := validSavedPlan()
	q := p
	q.Config = "a-secret-that-must-not-print"
	err := p.compare(&q)
	if err == nil || strings.Contains(err.Error(), q.Config) || !strings.Contains(err.Error(), "config_digest changed") {
		t.Fatal(err)
	}
}

func TestSavedPlanRedactsSecretValues(t *testing.T) {
	p := validSavedPlan()
	value := "secret-\"quoted\"-value"
	p.Changes = []plan.Change{{Kind: "service", Name: "web", Reasons: []string{"config " + value}}}
	store := secrets.New(map[string]string{"KEY": value})
	b, err := encodeSavedPlan(&p, store.RedactString)
	if err != nil {
		t.Fatal(err)
	}
	var got savedPlan
	if err = json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Changes[0].Reasons[0], value) {
		t.Fatal("secret leaked")
	}
	if !strings.Contains(got.Changes[0].Reasons[0], "***") {
		t.Fatalf("redaction missing: %s", b)
	}
}
