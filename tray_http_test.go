package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTrayLifecycle(t *testing.T) {
	store := testStore(t)
	now := time.Now()
	schedule := map[string][]CafeteriaMenu{"LUNCH": {{MenuID: 1, MenuName: "Рис"}, {MenuID: 2, MenuName: "Суп"}}}
	if err := store.saveCafeteriaWeek(context.Background(), []CafeteriaWeeklyPlan{
		{Date: "2026-11-21", MenusByMealType: schedule}, {Date: "2026-11-22", MenusByMealType: schedule},
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO cafeteria_images(dish_id,image_url,status,checked_at)
		SELECT id,'https://example.com/example.jpg','approved',0 FROM cafeteria_dishes`); err != nil {
		t.Fatal(err)
	}
	p := &trayPhotos{store: store, frames: t.TempDir(), now: func() time.Time { return now }, admins: map[int64]bool{123: true}}
	h := newMenuHandler(store, "token", p)
	post := "/api/tray?date=2026-11-21&meal=LUNCH"
	var frame bytes.Buffer
	if err := png.Encode(&frame, image.NewRGBA(image.Rect(0, 0, 12, 8))); err != nil {
		t.Fatal(err)
	}
	req := func(method, path string, user int64, body []byte, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		if user != 0 {
			r.Header.Set("X-Telegram-Init-Data", signedInitDataForUser("token", now, user))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, want, w.Body)
		}
		return w
	}
	req("POST", post, 0, frame.Bytes(), 401)
	req("POST", post, 321, []byte("invalid"), 415)
	req("POST", post, 321, frame.Bytes(), 201)
	req("POST", post, 456, frame.Bytes(), 409)
	req("GET", "/api/tray/1", 456, nil, 404)
	req("GET", "/api/tray/1/frame", 321, nil, 403)
	req("GET", "/api/tray/1/frame", 123, nil, 200)
	req("POST", "/api/tray/1/decision", 321, []byte(`{"crops":[]}`), 403)
	// A conflicting second crop must not publish the first.
	decision := []byte(`{"crops":[{"dish_id":1,"x":0,"y":0,"w":500,"h":1000},{"dish_id":2,"x":500,"y":0,"w":500,"h":1000}]}`)
	req("POST", "/api/tray/1/decision", 123, []byte(`{"crops":[{"dish_id":1,"w":500,"h":500},{"dish_id":99,"w":500,"h":500}]}`), 409)
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM tray_published`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial publication: %d %v", count, err)
	}
	req("POST", "/api/tray/1/decision", 123, decision, 204)
	var sub traySummary
	if err := json.Unmarshal(req("GET", "/api/tray/1", 321, nil, 200).Body.Bytes(), &sub); err != nil || len(sub.Result) != 2 {
		t.Fatalf("result: %+v %v", sub, err)
	}
	menu := func() []menuDish {
		var result struct{ Meals map[string][]menuDish }
		if err := json.Unmarshal(req("GET", "/api/menu?date=2026-11-22", 123, nil, 200).Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.Meals["LUNCH"]
	}
	d := menu()[0]
	if d.ImageKind != "cafeteria" || d.PhotoID == "" {
		t.Fatalf("not reused: %+v", d)
	}
	req("GET", d.ImageURL, 0, nil, 200)
	req("GET", "/moderation", 0, nil, 200) // The shell contains no private data.
	req("GET", "/api/tray?moderation=1", 0, nil, 401)
	req("GET", "/api/tray?moderation=1", 321, nil, 403)
	req("GET", "/api/tray?moderation=1", 123, nil, 200)
	req("GET", "/api/tray/published", 0, nil, 403)
	req("GET", "/api/tray/published", 321, nil, 403)
	w := req("GET", "/api/tray/published", 123, nil, 200)
	var published []trayDish
	if err := json.Unmarshal(w.Body.Bytes(), &published); err != nil || len(published) != 2 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("published moderator listing: %+v %v", published, err)
	}
	req("DELETE", "/api/tray/dish/1?photo_id="+d.PhotoID, 321, nil, 403)
	// Same JPEG/PNG bytes with a trailing byte are different source files.
	for i := byte(1); i <= 3; i++ {
		want := http.StatusCreated
		if i == 3 {
			want = http.StatusTooManyRequests
		}
		req("POST", post, 321, append(append([]byte{}, frame.Bytes()...), i), want)
	}
	req("POST", "/api/tray/2/decision", 123, decision, 409)
	replacement := fmt.Appendf(nil, `{"crops":[{"dish_id":1,"w":500,"h":500,"replace":%q}]}`, d.PhotoID)
	req("POST", "/api/tray/2/decision", 123, replacement, 204)
	req("GET", d.ImageURL, 0, nil, 404)
	req("DELETE", "/api/tray/dish/1?photo_id="+d.PhotoID, 123, nil, 409)
	d = menu()[0]
	req("DELETE", "/api/tray/dish/1?photo_id="+d.PhotoID, 123, nil, 204)
	if d := menu()[0]; d.ImageURL != "https://example.com/example.jpg" {
		t.Fatalf("no Wikimedia fallback: %+v", d)
	}
	now = now.Add(trayRetention + time.Second)
	req("GET", "/api/tray/1/frame", 123, nil, 404)
	if err := p.purgeTrayFrames(context.Background()); err != nil {
		t.Fatal(err)
	}
	req("GET", menu()[1].ImageURL, 0, nil, 200)
}

func TestTrayGlobalMealQuota(t *testing.T) {
	store := testStore(t)
	now := time.Now()
	const date, nextDate = "2026-11-21", "2026-11-22"
	meals := map[string][]CafeteriaMenu{
		"LUNCH": {{MenuID: 1, MenuName: "Рис"}}, "DINNER": {{MenuID: 2, MenuName: "Суп"}},
	}
	if err := store.saveCafeteriaWeek(context.Background(), []CafeteriaWeeklyPlan{
		{Date: date, MenusByMealType: meals}, {Date: nextDate, MenusByMealType: meals},
	}, now); err != nil {
		t.Fatal(err)
	}
	p := &trayPhotos{store: store, frames: t.TempDir(), now: func() time.Time { return now }}
	h := newMenuHandler(store, "token", p)
	var frame bytes.Buffer
	if err := png.Encode(&frame, image.NewRGBA(image.Rect(0, 0, 12, 8))); err != nil {
		t.Fatal(err)
	}
	upload := func(date, meal string, user int64, suffix byte) *httptest.ResponseRecorder {
		body := append(bytes.Clone(frame.Bytes()), suffix)
		r := httptest.NewRequest(http.MethodPost, "/api/tray?date="+date+"&meal="+meal, bytes.NewReader(body))
		r.Header.Set("X-Telegram-Init-Data", signedInitDataForUser("token", now, user))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	check := func(w *httptest.ResponseRecorder, want int) {
		t.Helper()
		if w.Code != want {
			t.Fatalf("upload: got %d want %d: %s", w.Code, want, w.Body)
		}
	}
	first := upload(date, "LUNCH", 1, 1)
	check(first, http.StatusCreated)
	var sub traySummary
	if err := json.Unmarshal(first.Body.Bytes(), &sub); err != nil {
		t.Fatal(err)
	}
	if err := p.decide(context.Background(), sub.ID, nil); err != nil {
		t.Fatal(err)
	}
	check(upload(date, "LUNCH", 2, 1), http.StatusConflict)

	// The two remaining places are shared among concurrent uploads from different users.
	const attempts = 5
	results := make(chan *httptest.ResponseRecorder, attempts)
	for i := byte(2); i < 2+attempts; i++ {
		go func() { results <- upload(date, "LUNCH", int64(i), i) }()
	}
	accepted := 0
	for range attempts {
		w := <-results
		if w.Code == http.StatusCreated {
			accepted++
			continue
		}
		check(w, http.StatusTooManyRequests)
		var outcome struct{ Code string }
		if err := json.Unmarshal(w.Body.Bytes(), &outcome); err != nil || outcome.Code != "meal_photo_limit" {
			t.Fatalf("expected the shared meal limit, got %s (%v)", w.Body, err)
		}
	}
	if accepted != 2 {
		t.Fatalf("accepted %d concurrent uploads; want 2", accepted)
	}

	check(upload(date, "DINNER", 1, 7), http.StatusCreated)
	check(upload(nextDate, "LUNCH", 1, 8), http.StatusCreated)
	daily := upload(nextDate, "DINNER", 1, 9)
	check(daily, http.StatusTooManyRequests)
	if daily.Body.String() != "daily frame limit reached\n" {
		t.Fatalf("expected the personal daily limit, got %s", daily.Body)
	}
	check(upload(nextDate, "DINNER", 2, 9), http.StatusCreated)
}
