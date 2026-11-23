package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestMenuModeratorRemovesDishImage(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Now()
	const first = "2026-11-21"
	const second = "2026-11-22"
	const example = "https://example.com/rice.jpg"
	schedule := map[string][]CafeteriaMenu{"LUNCH": {{MenuID: 1, MenuName: "Рис"}, {MenuID: 2, MenuName: "Суп"}}}
	plans := []CafeteriaWeeklyPlan{{Date: first, MenusByMealType: schedule}, {Date: second, MenusByMealType: schedule}}
	if err := store.saveCafeteriaWeek(ctx, plans, now); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO cafeteria_images(dish_id,image_url,author,status,checked_at)
		VALUES (1,?,'Photographer','approved',0),(2,?,'Photographer','approved',0)`, example, example)
	exec(`INSERT INTO cafeteria_cards(dish_id,description) VALUES (1,'Описание риса')`)
	exec(`INSERT INTO tray_published(dish_id,photo_id,jpeg) VALUES (1,'current-photo',X'00')`)
	exec(`UPDATE cafeteria_jobs SET status='running',lease_token='worker' WHERE kind='image' AND dish_id=1`)
	var jobID int64
	if err := store.db.QueryRow(`SELECT id FROM cafeteria_jobs WHERE dish_id=1 AND kind='image'`).Scan(&jobID); err != nil {
		t.Fatal(err)
	}

	p := &trayPhotos{store: store, now: func() time.Time { return now }, admins: map[int64]bool{123: true}}
	handler := newMenuHandler(store, "token", p)
	req := func(method, path string, user int64, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, nil)
		if user != 0 {
			r.Header.Set("X-Telegram-Init-Data", signedInitDataForUser("token", now, user))
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s as %d: got %d, want %d: %s", method, path, user, w.Code, want, w.Body)
		}
		return w
	}
	menu := func(date string, user int64) []menuDish {
		t.Helper()
		var data struct {
			Meals      map[string][]menuDish
			PhotoAdmin bool `json:"photo_admin"`
		}
		if err := json.Unmarshal(req("GET", "/api/menu?date="+date, user, 200).Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		if data.PhotoAdmin != (user == 123) {
			t.Fatalf("photo_admin for user %d: %v", user, data.PhotoAdmin)
		}
		return data.Meals["LUNCH"]
	}
	const path = "/api/menu/dish/1/image"
	const tray = path + "?photo_id=current-photo"
	req("DELETE", tray, 0, 403)
	req("DELETE", tray, 321, 403)
	req("DELETE", path, 123, 400)
	req("DELETE", path+"?photo_id=stale", 123, 409)
	req("DELETE", path+"?image_url="+url.QueryEscape(example), 123, 409) // A tray photo is now visible.
	if dish := menu(first, 321)[0]; dish.PhotoID != "current-photo" {
		t.Fatalf("rejected deletes changed the image: %+v", dish)
	}
	req("DELETE", tray, 123, 204)
	req("GET", "/api/tray-photo/current-photo", 0, 404)
	req("DELETE", tray, 123, 409)
	if err := store.saveCafeteriaImage(ctx, &cafeteriaJob{ID: jobID, DishID: 1, Token: "worker"}, cafeteriaImage{URL: example, Status: "approved"}); err == nil {
		t.Fatal("a running worker could still save an image after removal")
	}
	if err := store.saveCafeteriaWeek(ctx, plans, now); err != nil {
		t.Fatal(err)
	}
	for _, date := range []string{first, second} {
		items := menu(date, 321)
		if d := items[0]; d.Name != "Рис" || d.Description != "Описание риса" || d.ImageURL != "" || d.PhotoID != "" || d.ImageAuthor != "" {
			t.Fatalf("removed image or lost dish data on %s: %+v", date, d)
		}
		if items[1].ImageURL != example {
			t.Fatalf("unrelated dish lost its example: %+v", items[1])
		}
	}
	req("DELETE", "/api/menu/dish/2/image?image_url="+url.QueryEscape(example+"?stale"), 123, 409)
	req("DELETE", "/api/menu/dish/2/image?image_url="+url.QueryEscape(example), 123, 204)
	if d := menu(second, 123)[1]; d.ImageURL != "" || d.ImageAuthor != "" {
		t.Fatalf("Wikimedia example still visible: %+v", d)
	}
	exec(`UPDATE cafeteria_jobs SET status='done'`)
	exec(`UPDATE cafeteria_jobs SET status='pending' WHERE kind='image'`)
	if job, err := store.claimCafeteriaJob(ctx, now, "new-worker"); err != nil || job != nil {
		t.Fatalf("removed dishes must not be queued for automatic images: %+v, %v", job, err)
	}
	exec(`INSERT INTO tray_published(dish_id,photo_id,jpeg) VALUES (1,'new-approved-photo',X'00')`)
	if d := menu(second, 321)[0]; d.PhotoID != "new-approved-photo" || d.ImageKind != "cafeteria" {
		t.Fatalf("newly approved photo not visible: %+v", d)
	}
}
