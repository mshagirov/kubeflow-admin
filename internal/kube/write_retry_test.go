package kube

// write_retry_test.go proves that the write retry guard stops client-go from
// replaying a write after a 429 or 5xx response that carries a Retry-After
// header, while a read keeps its retry.
//
// The fixture is a real HTTP server and a real dynamic client built on a real
// rest configuration that carries the same guard the service installs. No
// cluster, kubeconfig or credential is involved: the server is local and the
// configuration names it directly.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/model"
)

// writeRetryCall is one recorded request.
type writeRetryCall struct {
	Method string
	Path   string
	Query  string
	Body   string
}

// writeRetryAPI is a minimal API server that records every request it
// receives and answers with a scripted response.
type writeRetryAPI struct {
	mu       sync.Mutex
	requests []writeRetryCall
	// answer writes the response for one request. The attempt number counts
	// every request the server received, so a replayed write can be answered
	// differently from the first attempt.
	answer func(w http.ResponseWriter, r *http.Request, call writeRetryCall, attempt int)
}

func (a *writeRetryAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	call := writeRetryCall{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(body)}
	a.mu.Lock()
	a.requests = append(a.requests, call)
	attempt := len(a.requests)
	a.mu.Unlock()
	a.answer(w, r, call, attempt)
}

// calls returns the recorded requests.
func (a *writeRetryAPI) calls() []writeRetryCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]writeRetryCall(nil), a.requests...)
}

// count returns the number of recorded requests with the given method.
func (a *writeRetryAPI) count(method string) int {
	n := 0
	for _, c := range a.calls() {
		if c.Method == method {
			n++
		}
	}
	return n
}

// deleteCounts returns the number of recorded delete requests, split into the
// dry runs and the real deletes.
func (a *writeRetryAPI) deleteCounts() (dry int, real int) {
	for _, c := range a.calls() {
		if c.Method != http.MethodDelete {
			continue
		}
		opts := metav1.DeleteOptions{}
		if err := json.Unmarshal([]byte(c.Body), &opts); err != nil {
			continue
		}
		if len(opts.DryRun) > 0 {
			dry++
			continue
		}
		real++
	}
	return dry, real
}

// newWriteRetryClient builds a real dynamic client on a real rest
// configuration that carries the write retry guard the service installs.
func newWriteRetryClient(t *testing.T, api *writeRetryAPI, wrap transport.WrapperFunc) dynamic.Interface {
	t.Helper()
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	cfg := &rest.Config{Host: server.URL, QPS: 5000, Burst: 5000}
	cfg.WrapTransport = disableWriteAutoRetry(wrap)
	d, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// writeRetryRoundTripper is a round tripper under the test control.
type writeRetryRoundTripper func(req *http.Request) (*http.Response, error)

func (f writeRetryRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// writeRetryStatus answers with a Kubernetes failure status.
func writeRetryStatus(w http.ResponseWriter, code int, reason metav1.StatusReason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure,
		Code:     int32(code),
		Reason:   reason,
		Message:  message,
	})
}

// writeRetrySuccess answers with a successful status.
func writeRetrySuccess(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusSuccess,
	})
}

// writeRetryProfile answers with the reviewed Profile object.
func writeRetryProfile(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(profileDeleteProfile("alice", "profile-1", "1").Object)
}

// writeRetryNotebooks answers with an empty Notebook list.
func writeRetryNotebooks(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "kubeflow.org/v1", "kind": "NotebookList", "items": []any{}})
}

// TestWriteIsNotReplayedAfterAServerError proves that a write is sent once
// when the API server answers 500 with a Retry-After header.
//
// The replay would be answered with 409, the conflict a write sees after the
// API server already applied the first attempt. Reporting that conflict
// would describe the replay, not the first attempt, so the write must never
// be replayed.
func TestWriteIsNotReplayedAfterAServerError(t *testing.T) {
	cases := []struct {
		name   string
		method string
		call   func(ctx context.Context, d dynamic.Interface) error
	}{
		{
			name:   "profile delete",
			method: http.MethodDelete,
			call: func(ctx context.Context, d dynamic.Interface) error {
				return d.Resource(Profiles).Delete(ctx, "alice", profileDeleteOptions(types.UID("profile-1"), "1", false))
			},
		},
		{
			name:   "profile delete dry run",
			method: http.MethodDelete,
			call: func(ctx context.Context, d dynamic.Interface) error {
				return d.Resource(Profiles).Delete(ctx, "alice", profileDeleteOptions(types.UID("profile-1"), "1", true))
			},
		},
		{
			name:   "profile patch",
			method: http.MethodPatch,
			call: func(ctx context.Context, d dynamic.Interface) error {
				_, err := d.Resource(Profiles).Patch(ctx, "alice", types.JSONPatchType, []byte(`[{"op":"replace","path":"/metadata/labels/tier","value":"gold"}]`), metav1.PatchOptions{})
				return err
			},
		},
		{
			name:   "profile create",
			method: http.MethodPost,
			call: func(ctx context.Context, d dynamic.Interface) error {
				_, err := d.Resource(Profiles).Create(ctx, profileDeleteProfile("alice", "profile-1", "1"), metav1.CreateOptions{})
				return err
			},
		},
		{
			name:   "profile update",
			method: http.MethodPut,
			call: func(ctx context.Context, d dynamic.Interface) error {
				_, err := d.Resource(Profiles).Update(ctx, profileDeleteProfile("alice", "profile-1", "2"), metav1.UpdateOptions{})
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &writeRetryAPI{}
			api.answer = func(w http.ResponseWriter, r *http.Request, call writeRetryCall, attempt int) {
				if attempt == 1 {
					w.Header().Set("Retry-After", "1")
					writeRetryStatus(w, http.StatusInternalServerError, metav1.StatusReasonInternalError, "the server could not apply the change")
					return
				}
				// A replayed write would land here and be reported as a
				// conflict, which is the failure this guard prevents.
				writeRetryStatus(w, http.StatusConflict, metav1.StatusReasonConflict, "the object has been modified")
			}
			d := newWriteRetryClient(t, api, nil)
			err := tc.call(context.Background(), d)
			if err == nil {
				t.Fatal("expected the write to fail")
			}
			if !apierrors.IsInternalError(err) {
				t.Fatalf("expected the first attempt error (500), got %v", err)
			}
			if apierrors.IsConflict(err) {
				t.Fatalf("the write was replayed and reported as a conflict: %v", err)
			}
			if n := api.count(tc.method); n != 1 {
				t.Fatalf("expected exactly one %s request, got %d", tc.method, n)
			}
		})
	}
}

// TestThrottledWriteIsNotReplayed proves that a throttled write is sent once.
func TestThrottledWriteIsNotReplayed(t *testing.T) {
	api := &writeRetryAPI{}
	api.answer = func(w http.ResponseWriter, r *http.Request, call writeRetryCall, attempt int) {
		w.Header().Set("Retry-After", "1")
		writeRetryStatus(w, http.StatusTooManyRequests, metav1.StatusReasonTooManyRequests, "too many requests")
	}
	d := newWriteRetryClient(t, api, nil)
	err := d.Resource(Profiles).Delete(context.Background(), "alice", profileDeleteOptions(types.UID("profile-1"), "1", false))
	if err == nil {
		t.Fatal("expected the write to fail")
	}
	if !apierrors.IsTooManyRequests(err) {
		t.Fatalf("expected the throttling error, got %v", err)
	}
	if n := api.count(http.MethodDelete); n != 1 {
		t.Fatalf("expected exactly one DELETE request, got %d", n)
	}
}

// TestWriteWithoutRetryAfterKeepsItsStatus proves that a response without a
// retry instruction is not changed: the status code the caller sees is the
// status code the API server sent.
func TestWriteWithoutRetryAfterKeepsItsStatus(t *testing.T) {
	cases := []struct {
		name   string
		code   int
		reason metav1.StatusReason
		check  func(error) bool
	}{
		{"server error", http.StatusInternalServerError, metav1.StatusReasonInternalError, apierrors.IsInternalError},
		{"throttled", http.StatusTooManyRequests, metav1.StatusReasonTooManyRequests, apierrors.IsTooManyRequests},
		{"conflict", http.StatusConflict, metav1.StatusReasonConflict, apierrors.IsConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &writeRetryAPI{}
			api.answer = func(w http.ResponseWriter, r *http.Request, call writeRetryCall, attempt int) {
				writeRetryStatus(w, tc.code, tc.reason, "the server refused the change")
			}
			d := newWriteRetryClient(t, api, nil)
			err := d.Resource(Profiles).Delete(context.Background(), "alice", profileDeleteOptions(types.UID("profile-1"), "1", false))
			if err == nil {
				t.Fatal("expected the write to fail")
			}
			if !tc.check(err) {
				t.Fatalf("expected the status %d to be reported, got %v", tc.code, err)
			}
			if n := api.count(http.MethodDelete); n != 1 {
				t.Fatalf("expected exactly one DELETE request, got %d", n)
			}
		})
	}
}

// TestReadKeepsItsRetry proves that the guard leaves a read alone: a throttled
// read is still retried and still succeeds.
func TestReadKeepsItsRetry(t *testing.T) {
	api := &writeRetryAPI{}
	api.answer = func(w http.ResponseWriter, r *http.Request, call writeRetryCall, attempt int) {
		if attempt == 1 {
			w.Header().Set("Retry-After", "0")
			writeRetryStatus(w, http.StatusTooManyRequests, metav1.StatusReasonTooManyRequests, "too many requests")
			return
		}
		writeRetryProfile(w)
	}
	d := newWriteRetryClient(t, api, nil)
	p, err := d.Resource(Profiles).Get(context.Background(), "alice", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected the read to be retried and to succeed: %v", err)
	}
	if p.GetName() != "alice" || p.GetUID() != "profile-1" {
		t.Fatalf("unexpected Profile: %+v", p.Object)
	}
	if n := api.count(http.MethodGet); n != 2 {
		t.Fatalf("expected exactly two GET requests, got %d", n)
	}
}

// TestProfileDeleteIsNotReplayed drives the real Profile deletion workflow
// against a real dynamic client.
//
// The dry run succeeds, the real delete answers 500 with a Retry-After header
// and a replay would answer 409. The delete must be sent once, and the
// outcome must stay unresolved for review: a 500 is not a definite refusal.
func TestProfileDeleteIsNotReplayed(t *testing.T) {
	f := newProfileDeleteFixture(t)
	var realDeletes atomic.Int32
	api := &writeRetryAPI{}
	api.answer = func(w http.ResponseWriter, r *http.Request, call writeRetryCall, attempt int) {
		switch {
		case r.Method == http.MethodDelete:
			opts := metav1.DeleteOptions{}
			_ = json.Unmarshal([]byte(call.Body), &opts)
			if len(opts.DryRun) > 0 {
				// A dry run never persists anything.
				writeRetrySuccess(w)
				return
			}
			if realDeletes.Add(1) == 1 {
				w.Header().Set("Retry-After", "1")
				writeRetryStatus(w, http.StatusInternalServerError, metav1.StatusReasonInternalError, "the server could not apply the deletion")
				return
			}
			// A replayed delete would land here and be reported as a
			// conflict.
			writeRetryStatus(w, http.StatusConflict, metav1.StatusReasonConflict, "the object has been modified")
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/profiles/alice"):
			writeRetryProfile(w)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/notebooks"):
			writeRetryNotebooks(w)
		default:
			writeRetryStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "missing")
		}
	}
	s := &Service{Config: f.service.Config, Typed: f.client, Dynamic: newWriteRetryClient(t, api, nil), PollInterval: f.service.PollInterval}
	p, err := s.prepareProfileDelete(context.Background(), profileDeleteAction())
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	dry, real := api.deleteCounts()
	if dry != 1 || real != 0 {
		t.Fatalf("prepare requests: dry=%d real=%d", dry, real)
	}
	err = s.executeProfileDelete(context.Background(), p, func(string) error { return nil })
	if err == nil {
		t.Fatal("expected the deletion to fail")
	}
	if apierrors.IsConflict(err) {
		t.Fatalf("the deletion was replayed and reported as a conflict: %v", err)
	}
	if !strings.Contains(err.Error(), "outcome is unknown") {
		t.Fatalf("expected an unresolved outcome, got %v", err)
	}
	if errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("an ambiguous outcome released the operation lock: %v", err)
	}
	dry, real = api.deleteCounts()
	if dry != 1 || real != 1 {
		t.Fatalf("execute requests: dry=%d real=%d", dry, real)
	}
}

// TestNewInstallsTheWriteRetryGuard proves that the service itself installs
// the guard: the clients the service builds from a configuration never replay
// a write.
func TestNewInstallsTheWriteRetryGuard(t *testing.T) {
	api := &writeRetryAPI{}
	api.answer = func(w http.ResponseWriter, r *http.Request, call writeRetryCall, attempt int) {
		if attempt == 1 {
			w.Header().Set("Retry-After", "1")
			writeRetryStatus(w, http.StatusInternalServerError, metav1.StatusReasonInternalError, "the server could not apply the change")
			return
		}
		writeRetryStatus(w, http.StatusConflict, metav1.StatusReasonConflict, "the object has been modified")
	}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	s, err := New(config.Config{
		Kubernetes: config.Kubernetes{Kubeconfig: writeRetryKubeconfig(t, server.URL)},
		Features:   config.Features{WritesEnabled: true, ProfileDeletion: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Dynamic.Resource(Profiles).Delete(context.Background(), "alice", profileDeleteOptions(types.UID("profile-1"), "1", false))
	if err == nil {
		t.Fatal("expected the write to fail")
	}
	if !apierrors.IsInternalError(err) {
		t.Fatalf("expected the first attempt error (500), got %v", err)
	}
	if apierrors.IsConflict(err) {
		t.Fatalf("the write was replayed and reported as a conflict: %v", err)
	}
	if n := api.count(http.MethodDelete); n != 1 {
		t.Fatalf("expected exactly one DELETE request, got %d", n)
	}
}

// writeRetryKubeconfig writes a minimal kubeconfig that names the local test
// server. It carries no credential and reaches no cluster.
func writeRetryKubeconfig(t *testing.T, server string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	content := "apiVersion: v1\n" +
		"kind: Config\n" +
		"clusters:\n" +
		"- cluster:\n" +
		"    server: " + server + "\n" +
		"  name: test\n" +
		"contexts:\n" +
		"- context:\n" +
		"    cluster: test\n" +
		"    user: test\n" +
		"  name: test\n" +
		"current-context: test\n" +
		"users:\n" +
		"- name: test\n" +
		"  user: {}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestWriteRetryGuardKeepsEverythingButTheRetryAfterHeader proves that the
// guard removes the retry instruction and nothing else: the status code, the
// other headers and the body are handed to the caller unchanged.
func TestWriteRetryGuardKeepsEverythingButTheRetryAfterHeader(t *testing.T) {
	body := `{"kind":"Status","status":"Failure"}`
	rt := writeRetryRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header: http.Header{
				"Content-Type":                   []string{"application/json"},
				"Retry-After":                    []string{"3"},
				"Warning":                        []string{"199 - the request was throttled"},
				"X-Kubernetes-Pf-Flowschema-Uid": []string{"flow-1"},
			},
			Body:    io.NopCloser(strings.NewReader(body)),
			Request: req,
		}, nil
	})
	guard := disableWriteAutoRetry(nil)(rt)
	req := httptest.NewRequest(http.MethodDelete, "https://api.example.test/apis/kubeflow.org/v1/profiles/alice", nil)
	resp, err := guard.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get(retryAfterHeader); got != "" {
		t.Fatalf("the retry instruction survived: %q", got)
	}
	for header, want := range map[string]string{
		"Content-Type":                   "application/json",
		"Warning":                        "199 - the request was throttled",
		"X-Kubernetes-Pf-Flowschema-Uid": "flow-1",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Fatalf("header %q = %q, want %q", header, got, want)
		}
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusInternalServerError)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != body {
		t.Fatalf("the body changed: %q", raw)
	}
}

// TestWriteRetryGuardLeavesSafeMethodsAlone proves that a safe read keeps
// its retry instruction.
func TestWriteRetryGuardLeavesSafeMethodsAlone(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			rt := writeRetryRoundTripper(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Header:     http.Header{retryAfterHeader: []string{"3"}},
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}, nil
			})
			guard := disableWriteAutoRetry(nil)(rt)
			req := httptest.NewRequest(method, "https://api.example.test/apis/kubeflow.org/v1/profiles/alice", nil)
			resp, err := guard.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			if got := resp.Header.Get(retryAfterHeader); got != "3" {
				t.Fatalf("the retry instruction of a safe read was removed: %q", got)
			}
		})
	}
}

// TestConfiguredTransportWrapperIsPreserved proves that the guard keeps the
// transport wrapper the configuration already carries.
func TestConfiguredTransportWrapperIsPreserved(t *testing.T) {
	var calls atomic.Int32
	existing := func(rt http.RoundTripper) http.RoundTripper {
		return writeRetryRoundTripper(func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			return rt.RoundTrip(req)
		})
	}
	api := &writeRetryAPI{}
	api.answer = func(w http.ResponseWriter, r *http.Request, call writeRetryCall, attempt int) {
		w.Header().Set("Retry-After", "1")
		writeRetryStatus(w, http.StatusInternalServerError, metav1.StatusReasonInternalError, "the server could not apply the change")
	}
	d := newWriteRetryClient(t, api, existing)
	err := d.Resource(Profiles).Delete(context.Background(), "alice", profileDeleteOptions(types.UID("profile-1"), "1", false))
	if err == nil || !apierrors.IsInternalError(err) {
		t.Fatalf("expected the first attempt error (500), got %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("expected the configured transport wrapper to be used once, got %d", n)
	}
	if n := api.count(http.MethodDelete); n != 1 {
		t.Fatalf("expected exactly one DELETE request, got %d", n)
	}
}
