package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"telegram-gemini-bot/internal/llm"
)

type queuedImageModel struct {
	responses []string
	calls     int
	cost      *float64
}

type blockingImageModel struct{ started, release chan struct{} }

func (m *blockingImageModel) Generate(ctx context.Context, _ llm.Request) (llm.Response, error) {
	close(m.started)
	select {
	case <-ctx.Done():
		return llm.Response{}, ctx.Err()
	case <-m.release:
		cost := 0.001
		return llm.Response{Text: `{"canonical_name":"","features":"","search_query":""}`, Usage: &llm.Usage{Cost: &cost}}, nil
	}
}

func TestImageJobFinishesAfterGracefulCancellation(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	if err := s.saveCafeteriaWeek(context.Background(), []CafeteriaWeeklyPlan{{Date: "2026-11-12", MenusByMealType: map[string][]CafeteriaMenu{"LUNCH": {{MenuID: 1, MenuName: "rice"}}}}}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE cafeteria_jobs SET status='done' WHERE kind='card'`); err != nil {
		t.Fatal(err)
	}
	model := &blockingImageModel{started: make(chan struct{}), release: make(chan struct{})}
	p := newCafeteriaPreparer(s, nil, Config{CafeteriaModel: "test"})
	p.model = model
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.drain(ctx); close(done) }()
	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("model not started")
	}
	cancel()
	close(model.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop")
	}
	var state string
	var cost sql.NullFloat64
	if err := s.db.QueryRow(`SELECT state,cost_usd FROM cafeteria_image_spend`).Scan(&state, &cost); err != nil || state != "completed" || !cost.Valid {
		t.Fatalf("state=%s cost=%v err=%v", state, cost, err)
	}
	var status, lastError string
	if err := s.db.QueryRow(`SELECT status,last_error FROM cafeteria_jobs WHERE kind='image'`).Scan(&status, &lastError); err != nil || status != "pending" || lastError != "" {
		t.Fatalf("status=%s error=%s err=%v", status, lastError, err)
	}
}

func TestRecoverStaleReservationAccountsForFullReserve(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.saveCafeteriaWeek(ctx, []CafeteriaWeeklyPlan{{Date: "2026-11-12", MenusByMealType: map[string][]CafeteriaMenu{"LUNCH": {{MenuID: 1, MenuName: "rice"}}}}}, now); err != nil {
		t.Fatal(err)
	}
	var dishID int64
	if err := s.db.QueryRow(`SELECT id FROM cafeteria_dishes`).Scan(&dishID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO cafeteria_image_spend(dish_id,purpose,state,reserved_usd,updated_at) VALUES(?,'vision','reserved',0.10,?),(?,'vision','reserved',0.10,?),(?,'vision','overrun',0.10,?)`, dishID, now.Add(-7*time.Minute).Unix(), dishID, now.Unix(), dishID, now.Add(-7*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE cafeteria_jobs SET status='pending',available_at=?,last_error=? WHERE kind='image'`, now.Add(6*time.Hour).Unix(), errImageBudgetBlocked.Error()); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverImageReservations(ctx, now); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Query(`SELECT state,cost_usd FROM cafeteria_image_spend ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var states []string
	for rows.Next() {
		var state string
		var cost sql.NullFloat64
		if err := rows.Scan(&state, &cost); err != nil {
			t.Fatal(err)
		}
		if cost.Valid {
			t.Fatal("estimated cost must remain unknown")
		}
		states = append(states, state)
	}
	rows.Close()
	if strings.Join(states, ",") != "estimated,reserved,overrun" {
		t.Fatalf("states=%v", states)
	}
	var available int64
	if err := s.db.QueryRow(`SELECT available_at FROM cafeteria_jobs WHERE kind='image'`).Scan(&available); err != nil || available != now.Unix() {
		t.Fatalf("available=%d err=%v", available, err)
	}
}

func TestEstimatedReservationCountsAgainstBudgetAndAllowsNextRequest(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO cafeteria_image_spend(dish_id,purpose,state,reserved_usd,updated_at) VALUES(1,'vision','estimated',0.10,0)`); err != nil {
		t.Fatal(err)
	}
	cost := 0.002
	model := &queuedImageModel{responses: []string{`{}`}, cost: &cost}
	p := newCafeteriaPreparer(s, nil, Config{})
	p.model = model
	if _, err := p.paidImageGenerate(ctx, 1, "vision", llm.Request{}); err != nil {
		t.Fatal(err)
	}
	var accounted float64
	if err := s.db.QueryRow(`SELECT sum(COALESCE(cost_usd,reserved_usd)) FROM cafeteria_image_spend`).Scan(&accounted); err != nil || accounted < 0.1019 || accounted > 0.1021 {
		t.Fatalf("accounted=%f err=%v", accounted, err)
	}
}

func (m *queuedImageModel) Generate(_ context.Context, _ llm.Request) (llm.Response, error) {
	if m.calls >= len(m.responses) {
		return llm.Response{}, errors.New("unexpected model request")
	}
	text := m.responses[m.calls]
	m.calls++
	return llm.Response{Text: text, Usage: &llm.Usage{Cost: m.cost, PromptTokens: 100, CompletionTokens: 20}}, nil
}

func TestVerifiedImageRejectsThenApprovesAndReuses(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.saveCafeteriaWeek(ctx, []CafeteriaWeeklyPlan{{Date: "2026-11-12", MenusByMealType: map[string][]CafeteriaMenu{"LUNCH": {{MenuID: 1, MenuName: "쌀밥"}}}}}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE cafeteria_jobs SET status='done' WHERE kind='card'`); err != nil {
		t.Fatal(err)
	}
	job, err := s.claimCafeteriaJob(ctx, now, "token")
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	body := `{"query":{"pages":[{"index":1,"title":"File:white rice beans.jpg","imageinfo":[{"url":"https://upload.wikimedia.org/a.jpg","descriptionurl":"https://commons.wikimedia.org/a","mime":"image/jpeg","extmetadata":{"LicenseShortName":{"value":"CC"},"LicenseUrl":{"value":"https://example.org/license"}}}]},{"index":2,"title":"File:white rice.jpg","imageinfo":[{"url":"https://upload.wikimedia.org/b.jpg","descriptionurl":"https://commons.wikimedia.org/b","mime":"image/jpeg","extmetadata":{"LicenseShortName":{"value":"CC"},"LicenseUrl":{"value":"https://example.org/license"}}}]}]}}`
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response := body
		if r.URL.Hostname() == "commons.wikimedia.org" && r.URL.Query().Get("titles") != "" {
			response = `{"query":{"pages":[{"imageinfo":[{"thumburl":"https://thumb.wikimedia.org/image.jpg"}]}]}}`
		}
		if r.URL.Hostname() == "thumb.wikimedia.org" {
			response = "\xff\xd8\xff\xe0" + strings.Repeat("x", 100)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
	})}
	cost := 0.001
	model := &queuedImageModel{cost: &cost, responses: []string{`{"canonical_name":"white rice","features":"plain boiled white rice","search_query":"white rice"}`, `{"decision":"reject","reason":"beans"}`, `{"decision":"approve","reason":"plain rice"}`}}
	p := newCafeteriaPreparer(s, client, Config{CafeteriaModel: "test"})
	p.model = model
	image, err := p.prepareVerifiedImage(ctx, job)
	if err != nil || image.Status != "approved" || image.Caption != "File:white rice.jpg" {
		t.Fatalf("image=%+v err=%v", image, err)
	}
	if model.calls != 3 {
		t.Fatalf("model calls=%d", model.calls)
	}
	image, err = p.prepareVerifiedImage(ctx, job)
	if err != nil || image.Status != "approved" || model.calls != 3 {
		t.Fatalf("reuse image=%+v calls=%d err=%v", image, model.calls, err)
	}
	var candidates, spend int
	if err := s.db.QueryRow(`SELECT count(*) FROM cafeteria_image_candidates WHERE decision IN ('approve','reject')`).Scan(&candidates); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM cafeteria_image_spend WHERE state='completed'`).Scan(&spend); err != nil {
		t.Fatal(err)
	}
	if candidates != 2 || spend != 3 {
		t.Fatalf("candidates=%d spend=%d", candidates, spend)
	}
}

func TestVerifiedImageUnknownCostStopsFurtherRequests(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	model := &queuedImageModel{responses: []string{`{}`}}
	p := newCafeteriaPreparer(s, nil, Config{})
	p.model = model
	_, err := p.paidImageGenerate(ctx, 1, "profile", llm.Request{})
	if !errors.Is(err, errImageBudgetBlocked) {
		t.Fatalf("expected unknown cost, got %v", err)
	}
	_, err = p.paidImageGenerate(ctx, 1, "profile", llm.Request{})
	if !errors.Is(err, errImageBudgetBlocked) || model.calls != 1 {
		t.Fatalf("budget not blocked: calls=%d err=%v", model.calls, err)
	}
}

func TestVerifiedImageOverrunStopsFurtherRequests(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cost := 0.11
	model := &queuedImageModel{responses: []string{`{}`}, cost: &cost}
	p := newCafeteriaPreparer(s, nil, Config{})
	p.model = model
	_, err := p.paidImageGenerate(ctx, 1, "profile", llm.Request{})
	if !errors.Is(err, errImageBudgetBlocked) {
		t.Fatalf("expected overrun, got %v", err)
	}
	_, err = p.paidImageGenerate(ctx, 1, "profile", llm.Request{})
	if !errors.Is(err, errImageBudgetBlocked) || model.calls != 1 {
		t.Fatalf("overrun not blocked: calls=%d err=%v", model.calls, err)
	}
}

func TestImageSearchQueriesStayBroadEnoughForVisualReview(t *testing.T) {
	query := conciseImageQuery("Ojingeo muguk Korean squid radish soup")
	if query != "squid radish soup" {
		t.Fatalf("query=%q", query)
	}
	if !candidateTitleMatches("File:Korean squid soup.jpg", query) {
		t.Fatal("expected two matching terms")
	}
	if candidateTitleMatches("File:cat.jpg", query) {
		t.Fatal("unrelated title matched")
	}
}

func TestVerifiedImageMigrationImportsOnlyMatchingPilotApprovals(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	plan := []CafeteriaWeeklyPlan{{Date: now.In(seoulLocation).Format("2006-01-02"), MenusByMealType: map[string][]CafeteriaMenu{"LUNCH": {{MenuID: 1, MenuName: "rice"}, {MenuID: 2, MenuName: "soup"}}}}}
	if err := s.saveCafeteriaWeek(ctx, plan, now); err != nil {
		t.Fatal(err)
	}
	_, err := s.db.Exec(`INSERT INTO cafeteria_images(dish_id,image_url,caption,status,checked_at) SELECT id,'https://example.com/old',name,'needs_review',0 FROM cafeteria_dishes`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`INSERT INTO cafeteria_image_pilot(dish_id,caption,image_url,source_url,author,license,license_url,thumbnail_url,model,prompt_version,state,decision,updated_at) SELECT id,name,'','','','','','https://example.com/thumb','model','vision-pilot-v1','completed',CASE name WHEN 'rice' THEN 'approve' ELSE 'reject' END,0 FROM cafeteria_dishes`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE cafeteria_jobs SET status='done' WHERE kind='image'`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`DELETE FROM cafeteria_migrations WHERE name='verified-images-v1'`); err != nil {
		t.Fatal(err)
	}
	if err := s.initVerifiedImages(); err != nil {
		t.Fatal(err)
	}
	meals, err := s.menuForDate(ctx, plan[0].Date)
	if err != nil {
		t.Fatal(err)
	}
	if meals["LUNCH"][0].ImageURL != "https://example.com/thumb" || meals["LUNCH"][1].ImageURL != "" {
		t.Fatalf("migration images %+v", meals["LUNCH"])
	}
	var pending int
	if err := s.db.QueryRow(`SELECT count(*) FROM cafeteria_jobs WHERE kind='image' AND status='pending'`).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("pending=%d err=%v", pending, err)
	}
}
