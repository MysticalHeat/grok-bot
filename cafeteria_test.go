package main

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"telegram-gemini-bot/internal/llm"
)

func testStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "test.db"), 10)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func countRows(t *testing.T, s *SQLiteStore, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWeekDeduplicationAndReplacement(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	rice := CafeteriaMenu{MenuID: 16, MenuName: "쌀밥", Course: "A"}
	otherCourse := rice
	otherCourse.Course = "B"
	plans := []CafeteriaWeeklyPlan{
		{Date: "2026-11-09", MenusByMealType: map[string][]CafeteriaMenu{"LUNCH": {rice, otherCourse}}},
		{Date: "2026-11-10", MenusByMealType: map[string][]CafeteriaMenu{"DINNER": {rice}}},
	}
	for i := 0; i < 2; i++ {
		if err := s.saveCafeteriaWeek(ctx, plans, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if got := countRows(t, s, "cafeteria_dishes"); got != 1 {
		t.Fatalf("dishes=%d", got)
	}
	if got := countRows(t, s, "cafeteria_schedule"); got != 3 {
		t.Fatalf("occurrences=%d", got)
	}
	if got := countRows(t, s, "cafeteria_jobs"); got != 2 {
		t.Fatalf("jobs=%d", got)
	}
	changed := rice
	changed.MenuName = "다른 음식"
	plans[0].MenusByMealType["LUNCH"] = []CafeteriaMenu{changed}
	if err := s.saveCafeteriaWeek(ctx, plans, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, s, "cafeteria_dishes"); got != 2 {
		t.Fatalf("changed name should create dish, got %d", got)
	}
	if got := countRows(t, s, "cafeteria_schedule"); got != 2 {
		t.Fatalf("stale schedule not removed: %d", got)
	}
	if got := countRows(t, s, "cafeteria_jobs"); got != 4 {
		t.Fatalf("changed name needs new jobs: %d", got)
	}
}

func TestAtomicJobClaimAndLeaseRecovery(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	plans := []CafeteriaWeeklyPlan{{Date: "2026-11-09", MenusByMealType: map[string][]CafeteriaMenu{"LUNCH": {{MenuID: 1, MenuName: "[안내]"}}}}}
	if err := s.saveCafeteriaWeek(ctx, plans, now); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	jobs := make(chan *cafeteriaJob, 2)
	errs := make(chan error, 2)
	for _, token := range []string{"one", "two"} {
		wg.Add(1)
		go func(token string) {
			defer wg.Done()
			j, err := s.claimCafeteriaJob(ctx, now, token)
			jobs <- j
			errs <- err
		}(token)
	}
	wg.Wait()
	close(jobs)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var original *cafeteriaJob
	for j := range jobs {
		if j != nil {
			if original != nil {
				t.Fatal("job claimed twice")
			}
			original = j
		}
	}
	if original == nil {
		t.Fatal("job not claimed")
	}
	reclaimed, err := s.claimCafeteriaJob(ctx, now.Add(6*time.Minute), "new")
	if err != nil || reclaimed == nil || reclaimed.ID != original.ID {
		t.Fatalf("reclaim=%+v err=%v", reclaimed, err)
	}
	if err := s.saveCafeteriaCard(ctx, original, cafeteriaCard{TranslatedName: "old"}, nil, "model"); err == nil {
		t.Fatal("stale worker saved result")
	}
	if err := s.saveCafeteriaCard(ctx, reclaimed, cafeteriaCard{TranslatedName: "new"}, nil, "model"); err != nil {
		t.Fatal(err)
	}
}

type fakeModel struct {
	text string
	err  error
}

func (m fakeModel) Generate(_ context.Context, req llm.Request) (llm.Response, error) {
	if req.ResponseSchema == nil {
		panic("missing structured schema")
	}
	return llm.Response{Text: m.text}, m.err
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func jsonClient(body string) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}
}

func TestCardInvalidJSONAndNoSource(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.saveCafeteriaWeek(ctx, []CafeteriaWeeklyPlan{{Date: "2026-11-09", MenusByMealType: map[string][]CafeteriaMenu{"LUNCH": {{MenuID: 16, MenuName: "쌀밥"}}}}}, now); err != nil {
		t.Fatal(err)
	}
	j, err := s.claimCafeteriaJob(ctx, now, "token")
	if err != nil || j == nil || j.Kind != "card" {
		t.Fatalf("job=%+v err=%v", j, err)
	}
	p := newCafeteriaPreparer(s, jsonClient(`{"query":{"pages":[{"title":"쌀밥","missing":true}]}}`), Config{CafeteriaModel: "test"})
	p.model = fakeModel{text: "not json"}
	if err := p.prepareCard(ctx, j); err == nil {
		t.Fatal("expected invalid JSON error")
	}
	if got := countRows(t, s, "cafeteria_cards"); got != 0 {
		t.Fatalf("invalid card saved: %d", got)
	}
	p.model = fakeModel{text: `{"translated_name":"Рис"}`}
	if err := p.prepareCard(ctx, j); err == nil {
		t.Fatal("expected missing required fields error")
	}
	p.model = fakeModel{text: `{"translated_name":"Рис","short_description":"Рис","description":"","typical_ingredients":["salt"],"needs_review":false,"review_reason":""}`}
	if err := p.prepareCard(ctx, j); err != nil {
		t.Fatal(err)
	}
	var review bool
	var ingredients string
	if err := s.db.QueryRow(`SELECT needs_review,typical_ingredients FROM cafeteria_cards WHERE dish_id=?`, j.DishID).Scan(&review, &ingredients); err != nil {
		t.Fatal(err)
	}
	if !review || ingredients != "null" {
		t.Fatalf("unverified card: review=%v ingredients=%s", review, ingredients)
	}
}

func TestImageCandidateAndMissingImage(t *testing.T) {
	ctx := context.Background()
	body := `{"query":{"pages":[{"title":"File:rice.jpg","imageinfo":[{"url":"https://example.org/rice.jpg","mime":"image/jpeg","descriptionurl":"https://example.org/page","extmetadata":{"SomeNumber":{"value":42},"Artist":{"value":"A"},"LicenseShortName":{"value":"CC BY 4.0"},"LicenseUrl":{"value":"https://example.org/license"}}}]}]}}`
	p := newCafeteriaPreparer(nil, jsonClient(body), Config{})
	img, err := p.searchImage(ctx, "쌀밥")
	if err != nil || img.Status != "needs_review" || img.Author != "A" || img.URL == "" {
		t.Fatalf("image=%+v err=%v", img, err)
	}
	p.http = jsonClient(`{"query":{"pages":[]}}`)
	img, err = p.searchImage(ctx, "unknown")
	if err != nil || img.Status != "not_found" {
		t.Fatalf("image=%+v err=%v", img, err)
	}
}

func TestTranslationSurvivesWikipediaRateLimit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.saveCafeteriaWeek(ctx, []CafeteriaWeeklyPlan{{Date: "2026-11-09", MenusByMealType: map[string][]CafeteriaMenu{"LUNCH": {{MenuID: 16, MenuName: "쌀밥"}}}}}, now); err != nil {
		t.Fatal(err)
	}
	j, err := s.claimCafeteriaJob(ctx, now, "token")
	if err != nil || j == nil {
		t.Fatalf("claim: %v", err)
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{"Retry-After": []string{"120"}}, Request: r}, nil
	})}
	p := newCafeteriaPreparer(s, client, Config{CafeteriaModel: "test"})
	p.model = fakeModel{text: `{"translated_name":"Рис","short_description":"","description":"","typical_ingredients":["salt"],"needs_review":false,"review_reason":""}`}
	if err := p.prepareCard(ctx, j); err != nil {
		t.Fatal(err)
	}
	var translation, ingredients string
	var review bool
	if err := s.db.QueryRow(`SELECT translated_name,typical_ingredients,needs_review FROM cafeteria_cards WHERE dish_id=?`, j.DishID).Scan(&translation, &ingredients, &review); err != nil {
		t.Fatal(err)
	}
	if translation != "Рис" || ingredients != "null" || !review {
		t.Fatalf("card: %s %s %v", translation, ingredients, review)
	}
	var until int64
	if err := s.db.QueryRow(`SELECT until FROM cafeteria_source_cooldowns WHERE host='ko.wikipedia.org'`).Scan(&until); err != nil || until < now.Add(110*time.Second).Unix() {
		t.Fatalf("cooldown: %d, %v", until, err)
	}
	var references int
	if err := s.db.QueryRow(`SELECT count(*) FROM cafeteria_jobs WHERE kind='reference' AND status='pending'`).Scan(&references); err != nil || references != 1 {
		t.Fatalf("reference retry: %d, %v", references, err)
	}
}

func TestImageCooldownPersistsAndSkipsJobs(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.saveCafeteriaWeek(ctx, []CafeteriaWeeklyPlan{{Date: "2026-11-09", MenusByMealType: map[string][]CafeteriaMenu{"LUNCH": {{MenuID: 1, MenuName: "rice"}}}}}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE cafeteria_jobs SET status='done' WHERE kind='card'`); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}
	p := newCafeteriaPreparer(s, client, Config{})
	if _, err := p.searchImage(ctx, "rice"); err == nil {
		t.Fatal("expected rate limit")
	}
	j, err := s.claimCafeteriaJob(ctx, now, "token")
	if err != nil || j != nil {
		t.Fatalf("cooldown not respected: job=%+v err=%v", j, err)
	}
	j, err = s.claimCafeteriaJob(ctx, now.Add(11*time.Minute), "token")
	if err != nil || j == nil || j.Kind != "image" {
		t.Fatalf("job not resumed: job=%+v err=%v", j, err)
	}
}

func TestImageFallbackStripsSauce(t *testing.T) {
	var searches []string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		searches = append(searches, r.URL.Query().Get("gsrsearch"))
		body := `{"query":{"pages":[]}}`
		if len(searches) == 2 {
			body = `{"query":{"pages":[{"title":"File:rice.jpg","imageinfo":[{"url":"https://example.org/rice.jpg","mime":"image/jpeg","extmetadata":{"LicenseShortName":{"value":"CC"},"LicenseUrl":{"value":"https://example.org/license"}}}]}]}}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	p := newCafeteriaPreparer(nil, client, Config{})
	img, err := p.searchImage(context.Background(), "rice*sauce")
	if err != nil || img.URL == "" || len(searches) != 2 || searches[1] != "rice" {
		t.Fatalf("image=%+v searches=%v err=%v", img, searches, err)
	}
}

func TestStorePersistsQueue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	s, err := NewSQLiteStore(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now()
	if err := s.saveCafeteriaWeek(ctx, []CafeteriaWeeklyPlan{{Date: "2026-11-09", MenusByMealType: map[string][]CafeteriaMenu{"LUNCH": {{MenuID: 16, MenuName: "쌀밥"}}}}}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = NewSQLiteStore(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var name string
	if err := s.db.QueryRow(`SELECT name FROM cafeteria_dishes`).Scan(&name); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	if name != "쌀밥" || countRows(t, s, "cafeteria_jobs") != 2 {
		t.Fatal("queue not persisted")
	}
}

func TestRequestedMenuQueuesWholeWeek(t *testing.T) {
	s := testStore(t)
	body := `[{"date":"2026-11-09","menusByMealType":{"LUNCH":[{"menuId":16,"menuName":"쌀밥","course":"A"}]}},{"date":"2026-11-10","menusByMealType":{"DINNER":[{"menuId":6,"menuName":"배추김치","course":"A"}]}}]`
	client := jsonClient(body)
	p := newCafeteriaPreparer(s, client, Config{})
	service := &BotService{httpClient: client, cafeteria: p}
	plan, err := service.fetchCafeteriaWeeklyPlan(context.Background(), "2026-11-09")
	if err != nil || plan.Date != "2026-11-09" {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if got := countRows(t, s, "cafeteria_dishes"); got != 2 {
		t.Fatalf("whole week dishes=%d", got)
	}
	if got := countRows(t, s, "cafeteria_jobs"); got != 4 {
		t.Fatalf("jobs=%d", got)
	}
	select {
	case <-p.wake:
	default:
		t.Fatal("worker was not notified")
	}
}
