package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	trayHistoryPath       = "/api/tray/1/history"
	trayHistoryValidReply = `{"suggestions":[{"dish_id":7,"x":0,"y":0,"w":500,"h":500,"selected":true,"reason":""}]}`
)

type trayHistoryHTTPEntry struct {
	StartedAt         time.Time `json:"started_at"`
	Attempt           int       `json:"attempt"`
	Model             string    `json:"model"`
	FinishReason      string    `json:"finish_reason"`
	Reason            string    `json:"reason"`
	Response          string    `json:"response"`
	ResponseTruncated bool      `json:"response_truncated"`
}

type trayHistoryFixture struct {
	p   *trayPhotos
	ai  *trayWorkerAI
	now time.Time
	h   http.Handler
}

func newTrayHistoryFixture(t *testing.T) *trayHistoryFixture {
	t.Helper()
	p, ai, now := trayWorkerFixture(t)
	f := &trayHistoryFixture{p: p, ai: ai, now: now, h: newMenuHandler(p.store, "token", p)}
	p.now = func() time.Time { return f.now }
	return f
}

func (f *trayHistoryFixture) request(t *testing.T, method, path string, user int64, want int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if user != 0 {
		r.Header.Set("X-Telegram-Init-Data", signedInitDataForUser("token", f.now, user))
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s %s as user %d: status %d, want %d: %s", method, path, user, w.Code, want, w.Body)
	}
	return w
}

func (f *trayHistoryFixture) pass(t *testing.T) {
	t.Helper()
	if err := f.p.trayWorkerPass(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (f *trayHistoryFixture) history(t *testing.T, count int) ([]trayHistoryHTTPEntry, string) {
	t.Helper()
	w := f.request(t, http.MethodGet, trayHistoryPath, 1, http.StatusOK)
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("history Cache-Control = %q, want no-store", got)
	}
	var entries []trayHistoryHTTPEntry
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil || entries == nil || len(entries) != count {
		t.Fatalf("history: want JSON array with %d entries, got %d: %.400s (decode error: %v)", count, len(entries), w.Body.String(), err)
	}
	return entries, w.Body.String()
}

func TestTrayHistoryRecordsRetryAndManualRescan(t *testing.T) {
	f := newTrayHistoryFixture(t)
	const broken = " \n{\"suggestions\":["
	const success = " \n```json\n" + trayHistoryValidReply + "\n```\n "
	firstScanAt := f.now
	f.ai.response, f.ai.retry, f.ai.finish = broken, success, "stop"
	f.pass(t)
	if f.ai.calls != 2 {
		t.Fatalf("first scan made %d AI calls, want 2", f.ai.calls)
	}
	first, _ := f.history(t, 2)
	if got := first[0]; got.Attempt != 2 || got.Response != success || got.Reason != "" || got.ResponseTruncated {
		t.Errorf("latest entry should be the literal successful retry: %+v", got)
	}
	if got := first[1]; got.Attempt != 1 || got.Response != broken || strings.TrimSpace(got.Reason) == "" || got.FinishReason != "stop" || got.ResponseTruncated {
		t.Errorf("first attempt should retain its literal malformed response, diagnostic and finish reason: %+v", got)
	}
	for _, entry := range first {
		if !entry.StartedAt.Equal(firstScanAt) || entry.Model != f.p.model {
			t.Errorf("attempt %d: started_at=%s model=%q, want %s and %q", entry.Attempt, entry.StartedAt, entry.Model, firstScanAt, f.p.model)
		}
	}

	f.now = f.now.Add(time.Minute)
	f.request(t, http.MethodPost, "/api/tray/1/rescan", 1, http.StatusAccepted)
	queued, _ := f.history(t, 2)
	if !slices.Equal(queued, first) || f.ai.calls != 2 {
		t.Fatalf("requeue changed history or called the model: %+v (AI calls: %d)", queued, f.ai.calls)
	}
	invalidDish := strings.Replace(trayHistoryValidReply, `"dish_id":7`, `"dish_id":999`, 1)
	f.ai.response, f.ai.retry, f.ai.finish = invalidDish, "", ""
	f.pass(t)
	after, _ := f.history(t, 3)
	if got := after[0]; got.Attempt != 1 || got.Response != invalidDish || strings.TrimSpace(got.Reason) == "" ||
		!got.StartedAt.Equal(f.now) || got.Model != f.p.model {
		t.Errorf("manual scan should restart at attempt 1 and record its validation diagnostic: %+v", got)
	}
	if !slices.Equal(after[1:], first) || f.ai.calls != 3 {
		t.Errorf("manual scan should append exactly one entry without changing past entries: %+v (AI calls: %d)", after, f.ai.calls)
	}
}

func TestTrayHistoryModeratorOnly(t *testing.T) {
	f := newTrayHistoryFixture(t)
	f.request(t, http.MethodGet, "/api/tray/42/history", 1, http.StatusNotFound)
	f.request(t, http.MethodGet, "/api/tray/invalid/history", 1, http.StatusNotFound)
	f.history(t, 0) // Before any call, an existing submission returns [] rather than null.

	const marker = "private-model-reply-for-history"
	// Put the marker in literal model text, not in a parsed suggestion field.
	f.ai.response = strings.TrimSuffix(trayHistoryValidReply, "}") + `,"extra":"` + marker + `"}`
	f.pass(t)
	for _, user := range []int64{99, 0} {
		w := f.request(t, http.MethodGet, trayHistoryPath, user, http.StatusForbidden)
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("history for user %d has Cache-Control %q, want no-store", user, got)
		}
	}
	if entries, _ := f.history(t, 1); entries[0].Response != f.ai.response {
		t.Fatal("moderator history did not contain the literal model reply")
	}

	for _, view := range []struct {
		path string
		user int64
	}{
		{"/api/tray/1", 99},
		{"/api/tray/1", 1},
		{"/api/tray", 99},
		{"/api/tray?moderation=1", 1},
	} {
		w := f.request(t, http.MethodGet, view.path, view.user, http.StatusOK)
		for _, private := range []string{marker, `"history"`, `"ai_history"`, `"response_truncated"`} {
			if strings.Contains(w.Body.String(), private) {
				t.Errorf("%s as user %d exposed history (%q): %s", view.path, view.user, private, w.Body)
			}
		}
	}
}

func TestTrayHistorySanitizesProviderError(t *testing.T) {
	const secret = "private-provider-error-sentinel"
	tests := []struct {
		name, reason string
		err          error
	}{
		{"service failure", "сервиса ИИ", errors.New(secret)},
		{"timeout", "90 секунд", errors.Join(errors.New(secret), context.DeadlineExceeded)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTrayHistoryFixture(t)
			f.ai.err = tt.err
			f.pass(t)
			entries, body := f.history(t, 1)
			got := entries[0]
			if got.Attempt != 1 || !strings.Contains(got.Reason, tt.reason) || got.Response != "" || strings.Contains(body, secret) {
				t.Fatalf("want %q and no provider error text or fabricated response, got %s", tt.reason, body)
			}
			var saved string
			if err := f.p.store.db.QueryRow(`SELECT reason || response || finish_reason FROM tray_ai_history WHERE tray_id=1`).Scan(&saved); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(saved, secret) {
				t.Fatal("private provider error persisted in SQLite")
			}
		})
	}
}

func TestTrayHistoryDoesNotRecordQuotaBlockedRetry(t *testing.T) {
	f := newTrayHistoryFixture(t)
	f.p.dailyCalls = 1
	f.ai.response = `{"suggestions":[`
	f.pass(t)
	entries, _ := f.history(t, 1)
	if got := entries[0]; f.ai.calls != 1 || got.Attempt != 1 || got.Response != f.ai.response {
		t.Fatalf("want only the actual request, got %+v after %d calls", got, f.ai.calls)
	}
}

func TestTrayHistoryTruncatesAtUTF8Boundary(t *testing.T) {
	f := newTrayHistoryFixture(t)
	const limit = 16 << 10
	const head = `{"suggestions":[{"dish_id":7,"x":0,"y":0,"w":500,"h":500,"selected":true,"reason":"`
	prefix := head + strings.Repeat("a", limit-1-len(head))
	// The first byte of 界 fits at the limit, but the full rune does not.
	f.ai.response = prefix + `界tail"}]}`
	f.pass(t)

	entries, _ := f.history(t, 1)
	got := entries[0]
	if !got.ResponseTruncated || got.Response != prefix || got.Reason != "" {
		t.Fatalf("want a UTF-8-safe %d-byte prefix of a successful response, got %d bytes, truncated=%v, reason=%q",
			len(prefix), len(got.Response), got.ResponseTruncated, got.Reason)
	}
}

func TestTrayHistoryExpiresPerCall(t *testing.T) {
	const ttl = 30 * 24 * time.Hour
	f := newTrayHistoryFixture(t)
	f.ai.response = trayHistoryValidReply
	f.pass(t)
	firstAt := f.now

	f.now = firstAt.Add(time.Hour)
	f.request(t, http.MethodPost, "/api/tray/1/rescan", 1, http.StatusAccepted)
	f.pass(t)
	beforeExpiry := firstAt.Add(ttl - time.Second)
	f.now = beforeExpiry
	f.history(t, 2)

	// No worker runs between the cutoff and this read.
	f.now = firstAt.Add(ttl + time.Second)
	entries, _ := f.history(t, 1)
	if want := firstAt.Add(time.Hour); !entries[0].StartedAt.Equal(want) {
		t.Fatalf("only the newer call should remain: started_at=%s, want %s", entries[0].StartedAt, want)
	}

	f.now = firstAt.Add(ttl + time.Hour + time.Second)
	f.pass(t)
	// Rewind so a read-time filter alone cannot hide any undeleted entries.
	f.now = beforeExpiry
	f.history(t, 0)
}

func TestTrayHistoryReturnsLatestTwentyForPhoto(t *testing.T) {
	f := newTrayHistoryFixture(t)
	f.p.dailyCalls = 30
	for i := 1; i <= 21; i++ {
		if i > 1 {
			f.request(t, http.MethodPost, "/api/tray/1/rescan", 1, http.StatusAccepted)
		}
		f.ai.response = strings.TrimSuffix(trayHistoryValidReply, "}") + fmt.Sprintf(`,"sequence":%d}`, i)
		f.pass(t)
	}
	entries, _ := f.history(t, 20)
	for i, entry := range entries {
		if want := fmt.Sprintf(`"sequence":%d}`, 21-i); !strings.HasSuffix(entry.Response, want) {
			t.Errorf("entry %d: response %q should end with %q", i, entry.Response, want)
		}
	}
	if _, err := f.p.store.db.Exec(`INSERT INTO tray_submissions(user_id,service_date,meal,upload_day,digest,frame,dishes,status,created_at)
		SELECT user_id,service_date,meal,upload_day,'other','',dishes,'review',created_at FROM tray_submissions WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	w := f.request(t, http.MethodGet, "/api/tray/2/history", 1, http.StatusOK)
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("another photo exposed history: %s", w.Body)
	}
}
