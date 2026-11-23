package main

import (
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"telegram-gemini-bot/internal/llm"
)

type trayWorkerAI struct {
	calls    int
	response string
	retry    string
	finish   string
	request  llm.Request
	err      error
}

func (a *trayWorkerAI) Generate(_ context.Context, r llm.Request) (llm.Response, error) {
	a.calls++
	a.request = r
	if a.calls > 1 && a.retry != "" {
		return llm.Response{Text: a.retry}, a.err
	}
	return llm.Response{Text: a.response, FinishReason: a.finish}, a.err
}

func trayWorkerFixture(t *testing.T) (*trayPhotos, *trayWorkerAI, time.Time) {
	t.Helper()
	s := testStore(t)
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	ai := &trayWorkerAI{}
	p := &trayPhotos{store: s, ai: ai, model: "test", dailyCalls: 20, frames: t.TempDir(), admins: map[int64]bool{1: true}, now: func() time.Time { return now }}
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	img.Set(0, 0, color.White)
	f, err := os.Create(filepath.Join(p.frames, "test.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if err = jpeg.Encode(f, img, nil); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err = s.db.Exec(`INSERT INTO tray_submissions(id,user_id,service_date,meal,upload_day,digest,frame,dishes,status,created_at) VALUES(1,99,'2026-11-21','LUNCH','2026-11-21','test','test.jpg','[{"id":7,"name":"rice"}]','queued',?)`, now.Unix()); err != nil {
		t.Fatal(err)
	}
	return p, ai, now
}

func TestTrayWorkerProcessesAndValidates(t *testing.T) {
	p, ai, _ := trayWorkerFixture(t)
	ai.response = `{"suggestions":[{"dish_id":7,"x":0,"y":0,"w":500,"h":500,"selected":true,"reason":"","replace":""}]}`
	if err := p.trayWorkerPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	var status, suggestions string
	if err := p.store.db.QueryRow(`SELECT status,suggestions FROM tray_submissions WHERE id=1`).Scan(&status, &suggestions); err != nil {
		t.Fatal(err)
	}
	if status != "review" || ai.calls != 1 || len(ai.request.Tools) != 0 || ai.request.ResponseSchema == nil {
		t.Fatalf("status=%s calls=%d request=%+v", status, ai.calls, ai.request)
	}
	if countRows(t, p.store, "tray_notifications") != 1 {
		t.Fatal("missing notification")
	}
	if len(suggestions) < 3 {
		t.Fatalf("empty suggestions: %s", suggestions)
	}
}

func TestTrayWorkerInvalidResponseAndRecovery(t *testing.T) {
	p, ai, _ := trayWorkerFixture(t)
	ai.response = `{"suggestions":[{"dish_id":999,"x":0,"y":0,"w":500,"h":500,"selected":true,"reason":"","replace":""}]}`
	if err := p.trayWorkerPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := p.store.db.QueryRow(`SELECT reason FROM tray_submissions WHERE id=1`).Scan(&reason); err != nil || !strings.Contains(reason, "блюда с ID 999 нет") {
		t.Fatalf("reason=%q err=%v", reason, err)
	}
	if _, err := p.store.db.Exec(`UPDATE tray_submissions SET status='queued',started_at=123`); err != nil {
		t.Fatal(err)
	}
	if err := p.recoverTrayClaims(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ai.calls != 1 {
		t.Fatal("recovery retried AI")
	}
	var status string
	if err := p.store.db.QueryRow(`SELECT status FROM tray_submissions`).Scan(&status); err != nil || status != "review" {
		t.Fatalf("status=%s err=%v", status, err)
	}
}

func TestTrayWorkerJSONRetry(t *testing.T) {
	const valid = `{"suggestions":[{"dish_id":7,"x":0,"y":0,"w":500,"h":500,"selected":true,"reason":""}]}`
	const broken = `{"suggestions":[{"dish_id":7,"x":0`
	tests := []struct {
		name, first, retry, finish, wantReason string
		limit, wantCalls                       int
	}{
		{"retry succeeds", broken, valid, "", "", 20, 2},
		{"empty reply gets a retry", " ", valid, "", "", 20, 2},
		{"fenced JSON needs no retry", " \n```JSON\r\n" + valid + "\r\n```\n ", "", "", "", 20, 1},
		{"extra text after fence is not accepted", "```json\n" + valid + "\n```\nDone", broken, "", "некорректный JSON", 20, 2},
		{"retry also fails", broken, broken, "", "некорректный JSON", 20, 2},
		{"retry respects quota", broken, valid, "", "некорректный JSON", 1, 1},
		{"length finish reason causes retry even with valid JSON", `{"suggestions":[]}`, valid, "length", "", 20, 2},
		{"length finish reason explains truncation when quota is exhausted", broken, valid, "length", "лимит длины ответа", 1, 1},
		{"valid JSON without suggestions needs no retry", `{"suggestions":[]}`, valid, "", "не предложил ни одной области", 20, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, ai, _ := trayWorkerFixture(t)
			p.dailyCalls, ai.response, ai.retry = tt.limit, tt.first, tt.retry
			ai.finish = tt.finish
			if err := p.trayWorkerPass(context.Background()); err != nil {
				t.Fatal(err)
			}
			var status, suggestions, reason string
			if err := p.store.db.QueryRow(`SELECT status,suggestions,reason FROM tray_submissions WHERE id=1`).Scan(&status, &suggestions, &reason); err != nil {
				t.Fatal(err)
			}
			var quota int
			if err := p.store.db.QueryRow(`SELECT calls FROM tray_ai_calls`).Scan(&quota); err != nil {
				t.Fatal(err)
			}
			if status != "review" || ai.calls != tt.wantCalls || quota != tt.wantCalls {
				t.Fatalf("status=%s requests=%d quota=%d; want review and %d calls", status, ai.calls, quota, tt.wantCalls)
			}
			if tt.wantCalls == 2 && ai.request.MaxTokens <= 2048 {
				t.Fatalf("retry did not increase the output budget: %d", ai.request.MaxTokens)
			}
			if tt.wantReason == "" {
				if reason != "" || !strings.Contains(suggestions, `"dish_id":7`) {
					t.Fatalf("missing saved suggestion: %s, reason=%q", suggestions, reason)
				}
			} else if suggestions != "[]" || !strings.Contains(reason, tt.wantReason) {
				t.Fatalf("suggestions=%s reason=%q; want %q and no crops", suggestions, reason, tt.wantReason)
			}
		})
	}
}

func TestTraySuggestionDiagnostics(t *testing.T) {
	const valid = `{"dish_id":7,"x":0,"y":0,"w":500,"h":500,"selected":true}`
	wrap := func(items string) string { return `{"suggestions":[` + items + `]}` }
	tests := []struct{ name, response, want string }{
		{"empty response", "", "пустой ответ"},
		{"bad JSON", `{"suggestions":[`, "некорректный JSON"},
		{"missing list", `{}`, "нет массива suggestions"},
		{"wrong list type", `{"suggestions":"text"}`, "с массивом suggestions"},
		{"empty list", wrap(""), "не предложил ни одной области"},
		{"too many items", wrap(strings.Repeat(valid+",", 32) + valid), "Слишком много предложений от ИИ: 33"},
		{"wrong coordinate type", wrap(strings.Replace(valid, `"x":0`, `"x":"0"`, 1)), "Предложение №1: поле x имеет неверный тип"},
		{"absent coordinate", wrap(strings.Replace(valid, `"x":0,`, "", 1)), "не хватает координат"},
		{"unknown dish", wrap(strings.Replace(valid, `"dish_id":7`, `"dish_id":99`, 1)), "блюда с ID 99 нет"},
		{"no dish selected", wrap(strings.Replace(valid, `"dish_id":7`, `"dish_id":0`, 1)), "выбрана область без указанного блюда"},
		{"right overflow", wrap(valid + "," + strings.Replace(valid, `"x":0`, `"x":900`, 1)), "Предложение №2: прямоугольник выходит за правый край"},
		{"bottom overflow", wrap(strings.Replace(valid, `"y":0`, `"y":900`, 1)), "выходит за нижний край"},
		{"invalid height", wrap(strings.Replace(valid, `"h":500`, `"h":0`, 1)), "высота (h) должна быть больше нуля"},
		{"duplicate selection", wrap(valid + "," + valid), "Предложение №2: для блюда с ID 7 выбрано несколько областей"},
		{"missing reason", wrap(strings.Replace(valid, `true`, `false`, 1)), "не объяснил, почему область не выбрана"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason, _ := parseTraySuggestions(tt.response, []trayDish{{ID: 7}})
			if len(got) != 0 || !strings.Contains(reason, tt.want) {
				t.Fatalf("crops=%+v, reason=%q; want %q", got, reason, tt.want)
			}
		})
	}
}

func TestTrayWorkerTimeoutDoesNotExposeProviderError(t *testing.T) {
	p, ai, _ := trayWorkerFixture(t)
	ai.err = errors.Join(errors.New("private provider details"), context.DeadlineExceeded)
	if err := p.trayWorkerPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := p.store.db.QueryRow(`SELECT reason FROM tray_submissions WHERE id=1`).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, "не ответил за 90 секунд") || strings.Contains(reason, "private") {
		t.Fatalf("unexpected diagnostic: %q", reason)
	}
}

func TestTrayWorkerBudgetAndNotifications(t *testing.T) {
	p, ai, now := trayWorkerFixture(t)
	p.dailyCalls = 1
	if _, err := p.store.db.Exec(`INSERT INTO tray_ai_calls VALUES(?,1)`, now.Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
	if err := p.trayWorkerPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ai.calls != 0 {
		t.Fatal("budget exceeded")
	}
	calls := 0
	p.notify = func(admin, id int64) error { calls++; return errors.New("temporary") }
	if err := p.processTrayNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.processTrayNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("backoff failed: %d", calls)
	}
	if _, err := p.store.db.Exec(`UPDATE tray_submissions SET status='closed'`); err != nil {
		t.Fatal(err)
	}
	if err := p.processTrayNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countRows(t, p.store, "tray_notifications") != 0 {
		t.Fatal("closed notification retained")
	}
}

func TestTrayWorkerPurgesClosedFrames(t *testing.T) {
	p, _, now := trayWorkerFixture(t)
	if _, err := p.store.db.Exec(`UPDATE tray_submissions SET status='closed',closed_at=?`, now.Add(-trayRetention-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := p.purgeTrayFrames(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.frames, "test.jpg")); !os.IsNotExist(err) {
		t.Fatalf("frame still exists: %v", err)
	}
	var frame string
	if err := p.store.db.QueryRow(`SELECT frame FROM tray_submissions`).Scan(&frame); err != nil || frame != "" {
		t.Fatalf("frame=%q err=%v", frame, err)
	}
}
