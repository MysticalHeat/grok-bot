package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

const (
	trayPublicationsURL     = "/api/tray/publications"
	trayPublicationDate     = "2026-11-21"
	trayPublicationMod      = 123
	trayPublicationUploader = 321
	trayPublicationOneCrop  = `{"crops":[{"dish_id":1,"x":100,"y":150,"w":400,"h":500}],"rotation":1}`
)

type trayPublicationFixture struct {
	p     *trayPhotos
	h     http.Handler
	now   time.Time
	frame []byte
}

func newTrayPublicationFixture(t *testing.T) *trayPublicationFixture {
	t.Helper()
	store := testStore(t)
	f := &trayPublicationFixture{now: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
	if err := store.saveCafeteriaWeek(context.Background(), []CafeteriaWeeklyPlan{{
		Date: trayPublicationDate,
		MenusByMealType: map[string][]CafeteriaMenu{"LUNCH": {
			{MenuID: 1, MenuName: "Rice"}, {MenuID: 2, MenuName: "Soup"}, {MenuID: 3, MenuName: "Salad"},
		}},
	}}, f.now); err != nil {
		t.Fatal(err)
	}
	f.p = &trayPhotos{
		store: store, frames: t.TempDir(), now: func() time.Time { return f.now },
		admins: map[int64]bool{trayPublicationMod: true},
	}
	f.h = newMenuHandler(store, "token", f.p)
	var frame bytes.Buffer
	if err := png.Encode(&frame, image.NewRGBA(image.Rect(0, 0, 120, 80))); err != nil {
		t.Fatal(err)
	}
	f.frame = frame.Bytes()
	return f
}

func (f *trayPublicationFixture) request(t *testing.T, method, path string, user int64, body []byte, want int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	if user != 0 {
		r.Header.Set("X-Telegram-Init-Data", signedInitDataForUser("token", f.now, user))
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s %s as user %d: got HTTP %d, want %d: %s", method, path, user, w.Code, want, w.Body)
	}
	return w
}

func decodeTrayPublicationResponse[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var result T
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v: %s", err, w.Body)
	}
	return result
}

func (f *trayPublicationFixture) upload(t *testing.T, suffix byte) string {
	t.Helper()
	// A trailing byte makes each upload distinct without changing the image.
	w := f.request(t, http.MethodPost, "/api/tray?date="+trayPublicationDate+"&meal=LUNCH",
		trayPublicationUploader, append(bytes.Clone(f.frame), suffix), http.StatusCreated)
	sub := decodeTrayPublicationResponse[traySummary](t, w)
	return fmt.Sprintf("/api/tray/%d", sub.ID)
}

type trayPublicationHTTPItem struct {
	ID             int64    `json:"id"`
	Date           string   `json:"date"`
	Meal           string   `json:"meal"`
	Result         []string `json:"result"`
	FrameAvailable bool     `json:"frame_available"`
}

type trayPublicationHTTPCrop struct {
	DishID   int64 `json:"dish_id"`
	X        int   `json:"x"`
	Y        int   `json:"y"`
	W        int   `json:"w"`
	H        int   `json:"h"`
	Rotation int   `json:"rotation"`
}

type trayPublicationHTTPDetail struct {
	traySummary
	Crops []trayPublicationHTTPCrop `json:"publication_crops"`
}

func (f *trayPublicationFixture) publications(t *testing.T) []trayPublicationHTTPItem {
	t.Helper()
	w := f.request(t, http.MethodGet, trayPublicationsURL, trayPublicationMod, nil, http.StatusOK)
	items := decodeTrayPublicationResponse[[]trayPublicationHTTPItem](t, w)
	if items == nil {
		t.Fatalf("publications must be an array: %s", w.Body)
	}
	return items
}

func TestTrayPublicationModeratorOnly(t *testing.T) {
	f := newTrayPublicationFixture(t)
	path := f.upload(t, 1)
	f.request(t, http.MethodPost, path+"/decision", trayPublicationMod, []byte(trayPublicationOneCrop), http.StatusNoContent)
	for _, user := range []int64{0, trayPublicationUploader} {
		t.Run(fmt.Sprintf("publications/user=%d", user), func(t *testing.T) {
			f.request(t, http.MethodGet, trayPublicationsURL, user, nil, http.StatusForbidden)
		})
		t.Run(fmt.Sprintf("republish/user=%d", user), func(t *testing.T) {
			f.request(t, http.MethodPost, path+"/republish", user, []byte(trayPublicationOneCrop), http.StatusForbidden)
		})
	}
}

func TestTrayPublicationHistory(t *testing.T) {
	f := newTrayPublicationFixture(t)
	published := f.upload(t, 1)
	rejected := f.upload(t, 2)
	f.upload(t, 3) // Still in review.
	f.request(t, http.MethodPost, rejected+"/decision", trayPublicationMod, []byte(`{"crops":[]}`), http.StatusNoContent)
	f.request(t, http.MethodPost, published+"/decision", trayPublicationMod, []byte(trayPublicationOneCrop), http.StatusNoContent)

	// Insert a pre-geometry publication using only columns that already existed.
	const legacyID = 42
	const legacyDate = "2026-11-20"
	if _, err := f.p.store.db.Exec(`INSERT INTO tray_submissions
		(id,user_id,service_date,meal,upload_day,digest,frame,dishes,status,result,created_at,closed_at)
		VALUES(?,?,?,'DINNER',?,'legacy','','[]','closed','["Legacy dish"]',?,?)`,
		legacyID, trayPublicationUploader, legacyDate, legacyDate, f.now.Unix(), f.now.Unix()); err != nil {
		t.Fatal(err)
	}
	items := f.publications(t)
	if len(items) != 2 {
		t.Fatalf("want only modern and legacy publications, got %+v", items)
	}
	for _, s := range items {
		switch {
		case fmt.Sprintf("/api/tray/%d", s.ID) == published:
			if s.Date != trayPublicationDate || s.Meal != "LUNCH" || !s.FrameAvailable || !slices.Equal(s.Result, []string{"Rice"}) {
				t.Errorf("incorrect current publication: %+v", s)
			}
		case s.ID == legacyID:
			if s.Date != legacyDate || s.Meal != "DINNER" || s.FrameAvailable || !slices.Equal(s.Result, []string{"Legacy dish"}) {
				t.Errorf("incorrect legacy publication: %+v", s)
			}
		default:
			t.Errorf("unexpected publication: %+v", s)
		}
	}
	if items[0].ID == items[1].ID {
		t.Fatalf("duplicate publication: %+v", items)
	}
	w := f.request(t, http.MethodGet, fmt.Sprintf("/api/tray/%d", legacyID), trayPublicationMod, nil, http.StatusOK)
	if s := decodeTrayPublicationResponse[trayPublicationHTTPDetail](t, w); len(s.Crops) != 0 {
		t.Errorf("legacy detail invented crop geometry: %+v", s.Crops)
	}
}

func TestTrayPublicationHistoryPagination(t *testing.T) {
	f := newTrayPublicationFixture(t)
	if _, err := f.p.store.db.Exec(`WITH RECURSIVE sequence(n) AS (
		SELECT 1 UNION ALL SELECT n+1 FROM sequence WHERE n<51
	) INSERT INTO tray_submissions
		(id,user_id,service_date,meal,upload_day,digest,frame,dishes,status,result,created_at,closed_at)
		SELECT n,?,?,'LUNCH',?,CAST(n AS TEXT),'','[]','closed','["Rice"]',?,? FROM sequence`,
		trayPublicationUploader, trayPublicationDate, trayPublicationDate, f.now.Unix(), f.now.Unix()); err != nil {
		t.Fatal(err)
	}
	first := f.publications(t)
	if len(first) != 50 || first[0].ID != 51 || first[49].ID != 2 {
		t.Fatalf("unexpected first history page: %+v", first)
	}
	w := f.request(t, http.MethodGet, trayPublicationsURL+"?before=2", trayPublicationMod, nil, http.StatusOK)
	if page := decodeTrayPublicationResponse[[]trayPublicationHTTPItem](t, w); len(page) != 1 || page[0].ID != 1 {
		t.Fatalf("unexpected second history page: %+v", page)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("publication history must not be cached")
	}
	f.request(t, http.MethodGet, trayPublicationsURL+"?before=-1", trayPublicationMod, nil, http.StatusBadRequest)
}

func TestTrayPublicationRepublish(t *testing.T) {
	f := newTrayPublicationFixture(t)
	ai := &trayWorkerAI{response: `{"suggestions":[{"dish_id":3,"x":50,"y":50,"w":300,"h":500,"selected":true}]}`}
	f.p.ai, f.p.model = ai, "test"
	path := f.upload(t, 1)
	if err := f.p.trayWorkerPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	post := func(action, body string, want int) {
		t.Helper()
		f.request(t, http.MethodPost, path+"/"+action, trayPublicationMod, []byte(body), want)
	}
	get := func(url string) *httptest.ResponseRecorder {
		t.Helper()
		return f.request(t, http.MethodGet, url, trayPublicationMod, nil, http.StatusOK)
	}
	photos := func() map[int64]string {
		t.Helper()
		m := map[int64]string{}
		for _, dish := range decodeTrayPublicationResponse[[]trayDish](t, get("/api/tray/published")) {
			m[dish.ID] = dish.PhotoID
		}
		return m
	}
	detail := func() trayPublicationHTTPDetail {
		t.Helper()
		return decodeTrayPublicationResponse[trayPublicationHTTPDetail](t, get(path))
	}
	checkCrops := func(got, want []trayPublicationHTTPCrop) {
		t.Helper()
		slices.SortFunc(got, func(a, b trayPublicationHTTPCrop) int { return int(a.DishID - b.DishID) })
		if !slices.Equal(got, want) {
			t.Errorf("saved crop geometry: got %+v, want %+v", got, want)
		}
	}
	post("decision", `{"rotation":1,"crops":[{"dish_id":1,"x":100,"y":150,"w":400,"h":500},{"dish_id":2,"x":550,"y":225,"w":350,"h":600}]}`, http.StatusNoContent)
	closedAt := f.now
	before := photos()
	if len(before) != 2 || before[1] == "" || before[2] == "" {
		t.Fatalf("initial publication did not create two photos: %+v", before)
	}
	f.request(t, http.MethodPatch, "/api/menu/dish/1/image?rotation=180&photo_id="+before[1], trayPublicationMod, nil, http.StatusNoContent)
	original := detail()
	one := trayPublicationHTTPCrop{DishID: 1, X: 100, Y: 150, W: 400, H: 500, Rotation: 1}
	two := trayPublicationHTTPCrop{DishID: 2, X: 550, Y: 225, W: 350, H: 600, Rotation: 1}
	checkCrops(original.Crops, []trayPublicationHTTPCrop{one, two})
	history := get(path + "/history").Body.String()
	originalFrame := get(path + "/frame").Body.Bytes()
	untouchedImage := get("/api/tray-photo/" + before[2]).Body.Bytes()
	queue := get("/api/tray?moderation=1").Body.String()
	originalCalls := ai.calls
	if originalCalls != 1 || len(original.Suggestions) != 1 {
		t.Fatalf("expected one existing scan with a suggestion; got %d calls and %+v", originalCalls, original.Suggestions)
	}

	f.now = f.now.Add(24 * time.Hour)
	post("republish", trayPublicationOneCrop, http.StatusConflict) // An existing photo needs a token.
	const recrop = `{"rotation":3,"crops":[{"dish_id":3,"x":600,"y":250,"w":300,"h":500},{"dish_id":1,"x":250,"y":125,"w":250,"h":500,"replace":%q}]}`
	// The first crop would add a photo if the stale token on the second crop
	// didn't reject the entire operation.
	post("republish", fmt.Sprintf(recrop, before[2]), http.StatusConflict)
	if got := photos(); !maps.Equal(got, before) {
		t.Fatalf("conflicted republish changed photos: before %v, after %v", before, got)
	}
	if failed := detail(); !slices.Equal(failed.Result, original.Result) {
		t.Errorf("conflicted republish changed result: before %v, after %v", original.Result, failed.Result)
	} else {
		checkCrops(failed.Crops, []trayPublicationHTTPCrop{one, two})
	}

	post("republish", fmt.Sprintf(recrop, before[1]), http.StatusNoContent)
	after := photos()
	if len(after) != 3 || after[1] == before[1] || after[1] == "" || after[2] != before[2] || after[3] == "" {
		t.Fatalf("want one replacement, one unchanged photo and one new photo: before %v, after %v", before, after)
	}
	var rotation int
	if err := f.p.store.db.QueryRow(`SELECT rotation FROM tray_published WHERE dish_id=1`).Scan(&rotation); err != nil || rotation != 0 {
		t.Fatalf("replacement inherited the old photo's orientation: got %d, err %v", rotation, err)
	}
	f.request(t, http.MethodGet, "/api/tray-photo/"+before[1], 0, nil, http.StatusNotFound)
	if !bytes.Equal(get("/api/tray-photo/"+after[2]).Body.Bytes(), untouchedImage) {
		t.Error("republish changed the untouched dish's image bytes")
	}
	w := f.request(t, http.MethodGet, "/api/tray-photo/"+after[1], 0, nil, http.StatusOK)
	cfg, err := jpeg.DecodeConfig(w.Body)
	if err != nil || cfg.Width != 20 || cfg.Height != 60 {
		t.Errorf("replacement JPEG: got %dx%d, want 20x60 from rotated frame (err: %v)", cfg.Width, cfg.Height, err)
	}
	if !bytes.Equal(get(path+"/frame").Body.Bytes(), originalFrame) {
		t.Error("republish changed the original frame")
	}
	edited := detail()
	checkCrops(edited.Crops, []trayPublicationHTTPCrop{
		{DishID: 1, X: 250, Y: 125, W: 250, H: 500, Rotation: 3},
		two,
		{DishID: 3, X: 600, Y: 250, W: 300, H: 500, Rotation: 3},
	})
	result := edited.Result
	slices.Sort(result)
	if edited.Status != "closed" || !slices.Equal(result, []string{"Rice", "Salad", "Soup"}) {
		t.Errorf("republish did not preserve closed status and extend result: %+v", edited)
	}
	w = f.request(t, http.MethodGet, path, trayPublicationUploader, nil, http.StatusOK)
	if fields := decodeTrayPublicationResponse[map[string]json.RawMessage](t, w); fields["publication_crops"] != nil {
		t.Errorf("uploader detail exposed moderator crops: %s", w.Body)
	}
	if get("/api/tray?moderation=1").Body.String() != queue || get(path+"/history").Body.String() != history ||
		ai.calls != originalCalls || !slices.Equal(original.Suggestions, edited.Suggestions) {
		t.Errorf("republish changed the moderation queue or AI state (model calls: %d → %d)", originalCalls, ai.calls)
	}

	// No purge is run: the disk file still exists, but eligibility must expire
	// based on the original close rather than the republish time.
	f.now = closedAt.Add(trayRetention + time.Second)
	f.request(t, http.MethodGet, path+"/frame", trayPublicationMod, nil, http.StatusNotFound)
	post("republish", fmt.Sprintf(`{"crops":[{"dish_id":1,"w":500,"h":500,"replace":%q}]}`, after[1]), http.StatusNotFound)
	items := f.publications(t)
	if len(items) != 1 || items[0].ID != edited.ID || items[0].FrameAvailable {
		t.Fatalf("expired originals should remain listed as unavailable: %+v", items)
	}
	slices.Sort(items[0].Result)
	if !slices.Equal(items[0].Result, result) {
		t.Errorf("publication list did not keep updated result: %v, want %v", items[0].Result, result)
	}
}

func TestTrayPublicationRepublishRejectsIneligible(t *testing.T) {
	f := newTrayPublicationFixture(t)
	path := f.upload(t, 1)
	republish := func(url, body string, want int) {
		t.Helper()
		f.request(t, http.MethodPost, url+"/republish", trayPublicationMod, []byte(body), want)
	}
	republish("/api/tray/999", trayPublicationOneCrop, http.StatusNotFound)
	republish(path, trayPublicationOneCrop, http.StatusConflict) // Still in review.
	f.request(t, http.MethodPost, path+"/decision", trayPublicationMod, []byte(`{"crops":[]}`), http.StatusNoContent)
	republish(path, trayPublicationOneCrop, http.StatusConflict) // Closed without a publication.

	published := f.upload(t, 2)
	f.request(t, http.MethodPost, published+"/decision", trayPublicationMod, []byte(trayPublicationOneCrop), http.StatusNoContent)
	republish(published, `{"crops":[]}`, http.StatusBadRequest)
	republish(published, `{"rotation":4,"crops":[{"dish_id":2,"w":500,"h":500}]}`, http.StatusBadRequest)

	// Simulate a lost file before its retention window ends.
	w := f.request(t, http.MethodGet, published, trayPublicationMod, nil, http.StatusOK)
	id := decodeTrayPublicationResponse[traySummary](t, w).ID
	var frame string
	if err := f.p.store.db.QueryRow(`SELECT frame FROM tray_submissions WHERE id=?`, id).Scan(&frame); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.p.frames, frame)); err != nil {
		t.Fatal(err)
	}
	republish(published, `{"crops":[{"dish_id":2,"w":500,"h":500}]}`, http.StatusNotFound)
	if items := f.publications(t); len(items) != 1 || items[0].FrameAvailable {
		t.Errorf("missing original should remain listed as unavailable: %+v", items)
	}
}
