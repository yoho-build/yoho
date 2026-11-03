package backup

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/config"
)

func entriesAt(days ...int) []Entry {
	base := time.Date(2026, 10, 31, 3, 0, 0, 0, time.UTC)
	var out []Entry
	for _, d := range days {
		ts := base.AddDate(0, 0, -d)
		out = append(out, Entry{ID: ts.Format(tsLayout), Time: ts})
	}
	return out
}

func entryDays(es []Entry) string {
	var s []string
	for _, e := range es {
		s = append(s, e.Time.Format("01-02"))
	}
	return strings.Join(s, ",")
}

func TestExpiredArchives(t *testing.T) {
	es := entriesAt(0, 1, 10, 29, 30, 31, 60) // newest first
	cases := []struct {
		name   string
		last   int
		within string
		want   string
	}{
		{"none", 0, "", ""},
		{"keep_last", 3, "", "10-02,10-01,09-30,09-01"},
		{"keep_within", 0, "30d", "09-30,09-01"},
		{"union", 6, "30d", "09-01"},
		{"union within wins", 2, "30d", "09-30,09-01"},
		{"hours", 0, "24h", "10-21,10-02,10-01,09-30,09-01"},
		{"months", 0, "1m", "09-30,09-01"},
		{"invalid keeps all", 0, "30x", ""},
	}
	for _, c := range cases {
		if got := entryDays(expiredArchives(es, c.last, c.within)); got != c.want {
			t.Errorf("%s: expired %q, want %q", c.name, got, c.want)
		}
	}
}

func TestResticKeepWithin(t *testing.T) {
	respond := dockerResponder(func(s string) (string, error) {
		if strings.Contains(s, "r backup --json") {
			return `{"message_type":"summary","snapshot_id":"abcdef0123456789","total_bytes_processed":1}`, nil
		}
		return "", nil
	})
	h := &fakeHost{respond: respond}
	o := baseOpts(h)
	o.Services = map[string]config.ServiceBackup{"app": {Volumes: []string{"storage"}}}
	o.Target = config.BackupTarget{Repository: "/srv/restic", KeepLast: 3, KeepWithin: "30d"}
	o.Password = "pw-value-123"
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if all := h.scripts(); !strings.Contains(all, "--group-by tags --keep-last 3 --keep-within '30d' --prune") {
		t.Fatalf("restic forget flags missing:\n%s", all)
	}

	h2 := &fakeHost{respond: respond}
	o = baseOpts(h2)
	o.Services = map[string]config.ServiceBackup{"app": {Volumes: []string{"storage"}}}
	o.Target = config.BackupTarget{Repository: "/srv/restic", KeepWithin: "1y6m"}
	o.Password = "pw-value-123"
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if all := h2.scripts(); !strings.Contains(all, "--group-by tags --keep-within '1y6m' --prune") || strings.Contains(all, "--keep-last") {
		t.Fatalf("restic forget flags:\n%s", all)
	}
}
