package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPilotBudgetReservesAndStopsOnUnknownCost(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.saveCafeteriaWeek(ctx, []CafeteriaWeeklyPlan{{Date: "2026-11-09", MenusByMealType: map[string][]CafeteriaMenu{"LUNCH": {{MenuID: 1, MenuName: "rice"}, {MenuID: 2, MenuName: "soup"}}}}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	rows, err := s.db.Query(`SELECT id FROM cafeteria_dishes ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	first := pilotCandidate{DishID: ids[0], Caption: "File:rice.jpg"}
	id, err := reservePilotRequest(ctx, s, first, "model")
	if err != nil {
		t.Fatal(err)
	}
	second := pilotCandidate{DishID: ids[1], Caption: "File:soup.jpg"}
	if _, err := reservePilotRequest(ctx, s, second, "model"); err == nil || !strings.Contains(err.Error(), "unreconciled") {
		t.Fatalf("expected refusal, got %v", err)
	}
	if _, err := s.db.Exec(`UPDATE cafeteria_image_pilot SET state='completed',cost_usd=0.01 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := reservePilotRequest(ctx, s, second, "model"); err != nil {
		t.Fatal(err)
	}
}

func TestPilotBudgetIsPersistent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.saveCafeteriaWeek(ctx, []CafeteriaWeeklyPlan{{Date: "2026-11-09", MenusByMealType: map[string][]CafeteriaMenu{"LUNCH": {{MenuID: 1, MenuName: "rice"}}}}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := s.db.QueryRow(`SELECT id FROM cafeteria_dishes`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	_, err := s.db.Exec(`INSERT INTO cafeteria_image_pilot(dish_id,caption,image_url,source_url,author,license,license_url,model,prompt_version,state,cost_usd,updated_at) VALUES(?,'old','','','','','','model',?,'completed',4.95,0)`, id, pilotVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reservePilotRequest(ctx, s, pilotCandidate{DishID: id, Caption: "new"}, "model"); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("expected budget refusal, got %v", err)
	}
}
