package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"
)

func signedInitData(token string, stamp time.Time) string {
	v := url.Values{"auth_date": {fmt.Sprint(stamp.Unix())}, "user": {`{"id":123,"first_name":"Test"}`}, "query_id": {"query"}}
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var fields []string
	for _, k := range keys {
		fields = append(fields, k+"="+v.Get(k))
	}
	secret := hmac.New(sha256.New, []byte("WebAppData"))
	secret.Write([]byte(token))
	mac := hmac.New(sha256.New, secret.Sum(nil))
	mac.Write([]byte(strings.Join(fields, "\n")))
	v.Set("hash", hex.EncodeToString(mac.Sum(nil)))
	return v.Encode()
}

func TestTelegramInitDataValidation(t *testing.T) {
	now := time.Now()
	valid := signedInitData("token", now)
	tests := []struct {
		name, data, token string
		want              bool
	}{
		{"valid", valid, "token", true},
		{"wrong token", valid, "other", false},
		{"tampered", valid + "&user=%7B%22id%22%3A999%7D", "token", false},
		{"expired", signedInitData("token", now.Add(-25*time.Hour)), "token", false},
		{"future", signedInitData("token", now.Add(time.Minute)), "token", false},
		{"missing", "", "token", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := validTelegramInitData(tc.data, tc.token, now); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMenuHandler(t *testing.T) {
	store := testStore(t)
	plan := []CafeteriaWeeklyPlan{{Date: "2026-11-12", MenusByMealType: map[string][]CafeteriaMenu{
		"LUNCH": {{MenuID: 1, MenuName: "밥", Course: "A"}, {MenuID: 2, MenuName: "국"}},
	}}}
	if err := store.saveCafeteriaWeek(context.Background(), plan, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, err := store.db.Exec(`INSERT INTO cafeteria_cards(dish_id,translated_name,short_description,typical_ingredients,sources,needs_review) SELECT id,'Рис','Описание','["рис"]','["https://example.com"]',1 FROM cafeteria_dishes WHERE external_id=1`)
	if err != nil {
		t.Fatal(err)
	}
	handler := newMenuHandler(store, "token")
	request := func(target, auth string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		r.Header.Set("X-Telegram-Init-Data", auth)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if got := request("/api/menu", "").Code; got != http.StatusUnauthorized {
		t.Fatalf("unauthorized: %d", got)
	}
	auth := signedInitData("token", time.Now())
	if got := request("/api/menu?date=2026-02-30", auth).Code; got != http.StatusBadRequest {
		t.Fatalf("invalid date: %d", got)
	}
	w := request("/api/menu?date=2026-11-12", auth)
	if w.Code != http.StatusOK {
		t.Fatalf("menu: %d %s", w.Code, w.Body)
	}
	var data struct {
		Date, Today string
		Meals       map[string][]menuDish
	}
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	items := data.Meals["LUNCH"]
	if data.Date != "2026-11-12" || len(items) != 2 || items[0].TranslatedName != "Рис" || items[0].Ingredients[0] != "рис" || !items[0].NeedsReview || items[1].Name != "국" {
		t.Fatalf("unexpected menu: %+v", data)
	}
	w = request("/api/menu?date=2026-11-13", auth)
	data.Meals = nil
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Meals) != 0 {
		t.Fatalf("expected empty date, got %+v", data.Meals)
	}
	w = request("/api/menu", auth)
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if want := time.Now().In(seoulLocation).Format("2006-01-02"); data.Date != want || data.Today != want {
		t.Fatalf("today: %+v, want %s", data, want)
	}
	if got := request("/", "").Code; got != http.StatusOK {
		t.Fatalf("assets: %d", got)
	}
}
