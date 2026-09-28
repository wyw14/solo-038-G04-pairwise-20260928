package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/wyw14/solo-038/internal/service"
)

func newTestRouter(t *testing.T) http.Handler {
	t.Helper()
	runtime, err := service.NewRuntime(service.Config{
		DataDir:           filepath.Join(t.TempDir(), "data"),
		FestoonCapacity:   120,
		RegisterGap:       500 * time.Millisecond,
		RegisterTolerance: 0.25,
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewRouter(runtime)
}

func doJSON(t *testing.T, router http.Handler, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var decoded map[string]any
	if response.Body.Len() > 0 {
		if !json.Valid(response.Body.Bytes()) {
			t.Fatalf("response was not JSON (status %d): %s", response.Code, response.Body.String())
		}
		_ = json.Unmarshal(response.Body.Bytes(), &decoded)
	}
	return response, decoded
}

func TestScheduleOrderLifecycleAPI(t *testing.T) {
	router := newTestRouter(t)
	now := time.Now().UTC()

	response, body := doJSON(t, router, http.MethodPost, "/api/schedule/orders", map[string]any{
		"due_at":                 now.Add(3 * time.Hour).Format(time.RFC3339),
		"print_duration_seconds": 3600,
		"plate":                  "PLATE-7",
		"colors":                 []string{"cyan", "black"},
		"dryer_target_temp":      65,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create order: status %d body %s", response.Code, response.Body.String())
	}
	order, _ := body["order"].(map[string]any)
	orderID, _ := order["id"].(string)
	if orderID == "" {
		t.Fatalf("missing order id in %v", body)
	}
	if order["print_duration_seconds"] != 3600.0 {
		t.Fatalf("duration not echoed in seconds: %v", order["print_duration_seconds"])
	}

	response, body = doJSON(t, router, http.MethodGet, "/api/schedule", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("get schedule: %d", response.Code)
	}
	if _, ok := body["impacts"].([]any); !ok {
		t.Fatalf("impacts should serialize as an array, got %T", body["impacts"])
	}

	response, _ = doJSON(t, router, http.MethodPost, "/api/schedule/orders/"+orderID+"/start", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("start order: %d body %s", response.Code, response.Body.String())
	}

	response, _ = doJSON(t, router, http.MethodPost, "/api/schedule/orders/"+orderID+"/complete", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("complete order: %d body %s", response.Code, response.Body.String())
	}

	// Revising a completed order must be a conflict.
	response, _ = doJSON(t, router, http.MethodPut, "/api/schedule/orders/"+orderID, map[string]any{
		"due_at":                 now.Add(4 * time.Hour).Format(time.RFC3339),
		"print_duration_seconds": 3600,
		"plate":                  "PLATE-7",
		"colors":                 []string{"cyan", "black"},
		"dryer_target_temp":      65,
	})
	if response.Code != http.StatusConflict {
		t.Fatalf("expected 409 revising completed order, got %d", response.Code)
	}

	// Unknown id maps to 404.
	response, _ = doJSON(t, router, http.MethodPost, "/api/schedule/orders/does-not-exist/start", nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown order, got %d", response.Code)
	}
}

func TestScheduleDowntimeAndValidationAPI(t *testing.T) {
	router := newTestRouter(t)
	now := time.Now().UTC()

	// Invalid ticket (zero duration) -> 400.
	response, _ := doJSON(t, router, http.MethodPost, "/api/schedule/orders", map[string]any{
		"due_at":            now.Add(time.Hour).Format(time.RFC3339),
		"plate":             "P",
		"colors":            []string{"black"},
		"dryer_target_temp": 60,
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid order, got %d", response.Code)
	}

	// Downtime with end before start -> 400.
	response, _ = doJSON(t, router, http.MethodPost, "/api/schedule/downtimes", map[string]any{
		"start":  now.Add(2 * time.Hour).Format(time.RFC3339),
		"end":    now.Add(time.Hour).Format(time.RFC3339),
		"reason": "bad window",
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for inverted downtime, got %d", response.Code)
	}

	response, body := doJSON(t, router, http.MethodPost, "/api/schedule/downtimes", map[string]any{
		"start":  now.Add(2 * time.Hour).Format(time.RFC3339),
		"end":    now.Add(3 * time.Hour).Format(time.RFC3339),
		"reason": "roller replacement",
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create downtime: %d body %s", response.Code, response.Body.String())
	}
	downtime, _ := body["downtime"].(map[string]any)
	id, _ := downtime["id"].(string)

	response, _ = doJSON(t, router, http.MethodGet, "/api/schedule/downtimes", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("list downtimes: %d", response.Code)
	}

	response, _ = doJSON(t, router, http.MethodDelete, "/api/schedule/downtimes/"+id, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("delete downtime: %d", response.Code)
	}
	response, _ = doJSON(t, router, http.MethodDelete, "/api/schedule/downtimes/"+id, nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("second delete should be 404, got %d", response.Code)
	}
}
