package setup

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/zk-drive/internal/setup"
	"github.com/kennguy3n/zk-drive/internal/storage"
)

// fakeService is an in-memory setupService for handler tests.
type fakeService struct {
	status      setup.Status
	statusErr   error
	completed   bool
	completeErr error
	marked      bool
}

func (f *fakeService) Status(context.Context) (setup.Status, error) {
	return f.status, f.statusErr
}
func (f *fakeService) IsCompleted(context.Context) (bool, error) {
	return f.completed, f.completeErr
}
func (f *fakeService) MarkCompleted(context.Context) error {
	if f.completeErr != nil {
		return f.completeErr
	}
	f.marked = true
	return nil
}

// fakeTester records the config it was asked to test and returns a
// canned error.
type fakeTester struct {
	called bool
	gotCfg storage.Config
	err    error
}

func (f *fakeTester) Test(_ context.Context, cfg storage.Config) error {
	f.called = true
	f.gotCfg = cfg
	return f.err
}

func TestStatusEndpoint(t *testing.T) {
	svc := &fakeService{status: setup.Status{SetupCompleted: false, NeedsSetup: true}}
	h := NewHandler(svc)

	req := httptest.NewRequest(http.MethodGet, "/api/setup/status", nil)
	rec := httptest.NewRecorder()
	h.Status(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}
	var got setup.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SetupCompleted || !got.NeedsSetup {
		t.Fatalf("unexpected status body: %+v", got)
	}
}

func TestStatusEndpointError(t *testing.T) {
	svc := &fakeService{statusErr: errors.New("boom")}
	h := NewHandler(svc)
	rec := httptest.NewRecorder()
	h.Status(rec, httptest.NewRequest(http.MethodGet, "/api/setup/status", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want 500", rec.Code)
	}
}

func TestTestStorageSuccess(t *testing.T) {
	svc := &fakeService{completed: false}
	tester := &fakeTester{}
	h := NewHandler(svc)
	h.withTester(tester)

	body := `{"endpoint":"https://s3.example.com","bucket":"b","access_key":"AK","secret_key":"SK","region":"eu-west-1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/setup/test-storage", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.TestStorage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	var res testStorageResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if !res.OK {
		t.Fatalf("expected ok=true, got %+v", res)
	}
	if !tester.called {
		t.Fatal("tester was not called")
	}
	if tester.gotCfg.Endpoint != "https://s3.example.com" || tester.gotCfg.Bucket != "b" ||
		tester.gotCfg.AccessKey != "AK" || tester.gotCfg.SecretKey != "SK" || tester.gotCfg.Region != "eu-west-1" {
		t.Fatalf("config not threaded through: %+v", tester.gotCfg)
	}
}

func TestTestStorageFailureReturns200WithError(t *testing.T) {
	svc := &fakeService{completed: false}
	tester := &fakeTester{err: errors.New("NoSuchBucket: the bucket does not exist\nstatus code: 404")}
	h := NewHandler(svc)
	h.withTester(tester)

	body := `{"endpoint":"https://s3.example.com","bucket":"b","access_key":"AK","secret_key":"SK"}`
	rec := httptest.NewRecorder()
	h.TestStorage(rec, httptest.NewRequest(http.MethodPost, "/api/setup/test-storage", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (connection failure is a user-correctable outcome)", rec.Code)
	}
	var res testStorageResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.OK {
		t.Fatal("expected ok=false")
	}
	if strings.Contains(res.Error, "\n") {
		t.Fatalf("error should be single-line, got %q", res.Error)
	}
	if !strings.Contains(res.Error, "NoSuchBucket") {
		t.Fatalf("expected reason in error, got %q", res.Error)
	}
}

func TestTestStorageMissingFields(t *testing.T) {
	svc := &fakeService{completed: false}
	tester := &fakeTester{}
	h := NewHandler(svc)
	h.withTester(tester)

	body := `{"endpoint":"https://s3.example.com"}`
	rec := httptest.NewRecorder()
	h.TestStorage(rec, httptest.NewRequest(http.MethodPost, "/api/setup/test-storage", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	var res testStorageResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.OK {
		t.Fatal("expected ok=false for missing fields")
	}
	if tester.called {
		t.Fatal("tester must NOT be called when required fields are missing")
	}
}

func TestTestStorageDisabledWhenComplete(t *testing.T) {
	svc := &fakeService{completed: true}
	tester := &fakeTester{}
	h := NewHandler(svc)
	h.withTester(tester)

	body := `{"endpoint":"https://s3.example.com","bucket":"b","access_key":"AK","secret_key":"SK"}`
	rec := httptest.NewRecorder()
	h.TestStorage(rec, httptest.NewRequest(http.MethodPost, "/api/setup/test-storage", strings.NewReader(body)))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403 once setup is complete", rec.Code)
	}
	if tester.called {
		t.Fatal("tester must NOT be called once setup is complete (SSRF guard)")
	}
}

func TestCompleteEndpoint(t *testing.T) {
	svc := &fakeService{}
	h := NewHandler(svc)
	rec := httptest.NewRecorder()
	h.Complete(rec, httptest.NewRequest(http.MethodPost, "/api/setup/complete", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if !svc.marked {
		t.Fatal("MarkCompleted was not called")
	}
}

func TestStorageTestErrorTruncates(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := storageTestError(errors.New(long))
	if len(got) > 300 {
		t.Fatalf("expected truncation to 300, got %d", len(got))
	}
	if storageTestError(nil) != "" {
		t.Fatal("nil error must map to empty string")
	}
}
