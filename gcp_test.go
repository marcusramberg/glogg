package main

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestToEntry(t *testing.T) {
	t.Parallel()
	payload, _ := structpb.NewStruct(map[string]any{
		"message": "boom",
		"user":    map[string]any{"id": 42},
	})
	e := toEntry(&loggingpb.LogEntry{
		Severity: ltype.LogSeverity_ERROR,
		Labels:   map[string]string{"k8s-pod/app": "api"},
		Payload:  &loggingpb.LogEntry_JsonPayload{JsonPayload: payload},
	})
	if e.sev != "ERROR" || e.msg != "boom" {
		t.Fatalf("got sev=%q msg=%q", e.sev, e.msg)
	}
	for _, want := range []string{
		`jsonPayload.user.id="42"`,
		`labels."k8s-pod/app"="api"`,
		`severity="ERROR"`,
	} {
		if !slices.ContainsFunc(e.fields, func(f field) bool { return f.expr() == want }) {
			t.Errorf("missing %s in %v", want, e.fields)
		}
	}
}

func TestBuildFilter(t *testing.T) {
	t.Parallel()
	got := buildFilter("a OR b", time.Unix(0, 0), time.Time{}, "WARNING")
	want := `(a OR b) AND timestamp>="1970-01-01T00:00:00Z" AND severity>=WARNING`
	if got != want {
		t.Errorf("got %s", got)
	}
	got = buildFilter("", time.Unix(0, 0), time.Unix(60, 0), "")
	want = `timestamp>="1970-01-01T00:00:00Z" AND timestamp<="1970-01-01T00:01:00Z"`
	if got != want {
		t.Errorf("got %s", got)
	}
}

func TestProjectsCache(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "glogg", "projects.json")
	if got := loadProjects(path); len(got) != 0 {
		t.Fatalf("got %v from a missing cache", got)
	}
	want := []field{{"a-proj", "A"}, {"b-proj", "B"}}
	if err := saveProjects(path, want); err != nil {
		t.Fatal(err)
	}
	if got := loadProjects(path); !slices.Equal(got, want) {
		t.Errorf("got %v", got)
	}
}
