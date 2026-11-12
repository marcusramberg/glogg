package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	logging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	"cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/iterator"
	_ "google.golang.org/genproto/googleapis/cloud/audit" // lets protojson resolve audit log protoPayloads
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

const pageSize = 200

// field is one attribute of a log entry; k is a path usable in the Logging query language.
type field struct{ k, v string }

type entry struct {
	ts            time.Time
	sev, src, msg string
	fields        []field
}

type gcp struct {
	logs     *logging.Client
	projects *resourcemanager.ProjectsClient
	// sharedQuota: gcloud user credentials without a quota project draw on a read quota
	// shared with every other gcloud user, which is routinely exhausted.
	sharedQuota bool
}

func newGCP(ctx context.Context) (*gcp, error) {
	logs, err := logging.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	projects, err := resourcemanager.NewProjectsClient(ctx)
	if err != nil {
		return nil, err
	}
	var adc struct {
		Type  string `json:"type"`
		Quota string `json:"quota_project_id"`
	}
	if creds, err := google.FindDefaultCredentials(ctx); err == nil {
		_ = json.Unmarshal(creds.JSON, &adc)
	}
	sharedQuota := adc.Type == "authorized_user" && adc.Quota == "" && os.Getenv("GOOGLE_CLOUD_QUOTA_PROJECT") == ""
	return &gcp{logs, projects, sharedQuota}, nil
}

// entries fetches one page (possibly short or empty), newest first, and the token for the next page.
func (g *gcp) entries(project, filter, token string) ([]entry, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	it := g.logs.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{"projects/" + project},
		Filter:        filter,
		OrderBy:       "timestamp desc",
	})
	// Exactly one request: the API returns short or empty pages while it scans for sparse
	// matches, and iterator.Pager would loop on those until the per-minute read quota is gone.
	page, next, err := it.InternalFetch(pageSize, token)
	if g.sharedQuota && status.Code(err) == codes.ResourceExhausted {
		return nil, "", fmt.Errorf("gcloud's shared quota is exhausted, run: gcloud auth application-default set-quota-project PROJECT (%w)", err)
	}
	if err != nil {
		return nil, "", err
	}
	out := make([]entry, len(page))
	for i, e := range page {
		out[i] = toEntry(e)
	}
	return out, next, nil
}

// listProjects returns every active project the credentials can see, as {id, display name}.
func (g *gcp) listProjects() ([]field, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	it := g.projects.SearchProjects(ctx, &resourcemanagerpb.SearchProjectsRequest{Query: "state:ACTIVE"})
	var out []field
	for {
		p, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		out = append(out, field{p.ProjectId, p.DisplayName})
	}
	slices.SortFunc(out, func(a, b field) int { return strings.Compare(a.k, b.k) })
	return out, nil
}

func toEntry(e *loggingpb.LogEntry) entry {
	var fs []field
	// protojson names fields exactly as the query language does (jsonPayload, resource.labels.x, ...)
	b, err := protojson.Marshal(e)
	if err == nil {
		var m map[string]any
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		if err = dec.Decode(&m); err == nil {
			flatten("", m, &fs)
		}
	}
	if err != nil {
		fs = []field{{"error", err.Error()}}
	}
	msg := first(fs, "textPayload", "jsonPayload.message", "jsonPayload.msg", "protoPayload.methodName", "httpRequest.requestUrl", "error")
	if msg == "" {
		var parts []string
		for _, f := range fs {
			if k, ok := strings.CutPrefix(f.k, "jsonPayload."); ok {
				parts = append(parts, k+"="+f.v)
			}
		}
		msg = strings.Join(parts, " ")
	}
	return entry{
		ts:     e.GetTimestamp().AsTime(),
		sev:    e.GetSeverity().String(),
		src:    first(fs, "resource.labels.container_name", "resource.labels.service_name", "resource.labels.function_name", "resource.labels.job_name", "resource.type"),
		msg:    msg,
		fields: fs,
	}
}

func first(fs []field, keys ...string) string {
	for _, k := range keys {
		for _, f := range fs {
			if f.k == k && f.v != "" {
				return f.v
			}
		}
	}
	return ""
}

var bareKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func flatten(prefix string, v any, out *[]field) {
	switch v := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			p := k
			if !bareKey.MatchString(k) {
				p = strconv.Quote(k) // e.g. labels."k8s-pod/app"
			}
			if prefix != "" {
				p = prefix + "." + p
			}
			flatten(p, v[k], out)
		}
	case nil:
	case string:
		*out = append(*out, field{prefix, v})
	case []any:
		// shortcut: arrays are shown as JSON and can't be filtered on by equality; flatten per element if that's needed
		b, _ := json.Marshal(v)
		*out = append(*out, field{prefix, string(b)})
	default: // json.Number, bool
		*out = append(*out, field{prefix, fmt.Sprint(v)})
	}
}

// expr is the query-language term matching this field exactly.
func (f field) expr() string { return f.k + "=" + strconv.Quote(f.v) }

func buildFilter(user string, since time.Time, minSeverity string) string {
	var parts []string
	if user != "" {
		parts = append(parts, "("+user+")")
	}
	parts = append(parts, `timestamp>="`+since.UTC().Format(time.RFC3339)+`"`)
	if minSeverity != "" {
		parts = append(parts, "severity>="+minSeverity)
	}
	return strings.Join(parts, " AND ")
}
